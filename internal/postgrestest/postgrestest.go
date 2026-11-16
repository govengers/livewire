// Package postgrestest starts the embedded PostgreSQL 18 of a test binary (TST-002) for
// livewire's own suites: the postgresadapter and query packages, the e2e suite and the
// example's tests. Test support only; nothing outside the tests imports it.
//
// The binaries are unpacked once per machine and PostgreSQL version into the directory
// embedded-postgres caches its downloads in (~/.embedded-postgres-go, which the govengers CI
// restores), and every test binary runs its instance in a directory of its package's own
// below it (D89). Unpacking inside a -race test binary takes about 40 s; a start from the
// unpacked binaries takes a few seconds.
package postgrestest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
)

const version = embeddedpostgres.V18

// Server is a running embedded PostgreSQL; user and password are postgres.
type Server struct {
	// Port is the TCP port it listens on at 127.0.0.1.
	Port      int
	postgres  *embeddedpostgres.EmbeddedPostgres
	directory string
}

// Start starts a PostgreSQL on a free port; the caller stops it with Stop once its tests ran.
// It first stops the instances of this package's test binaries that died before they could
// stop theirs (a killed binary, a test timeout).
func Start() (*Server, error) {
	instances := instancesDirectory()
	if err := os.MkdirAll(instances, 0o700); err != nil {
		return nil, err
	}
	stopOrphans(instances)
	binaries, err := unpackedBinaries()
	if err != nil {
		return nil, err
	}
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	directory, err := os.MkdirTemp(instances, instancePrefix)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(directory, ownerFile), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		_ = os.RemoveAll(directory) // nothing runs from it yet
		return nil, err
	}
	postgres := newDatabase(port, binaries, filepath.Join(directory, "runtime"))
	if err := postgres.Start(); err != nil {
		_ = os.RemoveAll(directory) // the start failed; the next sweep would remove it too
		return nil, err
	}
	return &Server{Port: port, postgres: postgres, directory: directory}, nil
}

// URL is the connection URL of database on the server.
func (s *Server) URL(database string) string {
	return fmt.Sprintf("postgres://postgres:postgres@127.0.0.1:%d/%s", s.Port, database)
}

// Stop stops the server and removes its instance directory.
func (s *Server) Stop() error {
	err := s.postgres.Stop()
	_ = os.RemoveAll(s.directory) // a leftover directory is swept by the next Start
	return err
}

// instancePrefix names the instance directories; ownerFile in each holds the process ID of
// the test binary that started it.
const (
	instancePrefix = "instance-"
	ownerFile      = "owner"
)

func newDatabase(port int, binaries, runtime string) *embeddedpostgres.EmbeddedPostgres {
	return embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Version(version).
		Port(uint32(port)). //nolint:gosec // G115: a TCP port fits a uint32
		BinariesPath(binaries).
		RuntimePath(runtime).
		Logger(io.Discard))
}

// stopOrphans stops the instances in directory whose test binary no longer runs and removes
// their directories, so neither processes nor directories pile up.
func stopOrphans(directory string) {
	found, _ := filepath.Glob(filepath.Join(directory, instancePrefix+"*")) // the pattern is valid
	for _, instance := range found {
		owner, err := os.ReadFile(filepath.Join(instance, ownerFile)) //nolint:gosec // G304: a directory of this harness, found by its prefix
		if err != nil || alive(string(owner)) {
			continue // being created, or its test binary still runs
		}
		if postmaster, err := os.ReadFile(filepath.Join(instance, "runtime", "data", "postmaster.pid")); err == nil { //nolint:gosec // G304: as above
			processID := strings.TrimSpace(strings.SplitN(string(postmaster), "\n", 2)[0])
			if runsFrom(processID, instance) {
				_ = signal(processID, os.Interrupt) // fast shutdown; gone already is fine
			}
		}
		_ = os.RemoveAll(instance) // a leftover directory is harmless
	}
}

func alive(processID string) bool { return signal(processID, syscall.Signal(0)) == nil }

// runsFrom reports whether the process is still the PostgreSQL of directory: after its
// postmaster died, its process ID may belong to an unrelated process.
func runsFrom(processID, directory string) bool {
	if _, err := strconv.Atoi(processID); err != nil {
		return false
	}
	command, err := exec.Command("ps", "-o", "command=", "-p", processID).Output() //nolint:gosec,noctx // G204: a process ID PostgreSQL wrote; ps returns at once
	return err == nil && strings.Contains(string(command), directory)
}

func signal(processID string, sent os.Signal) error {
	number, err := strconv.Atoi(strings.TrimSpace(processID))
	if err != nil {
		return err
	}
	process, err := os.FindProcess(number)
	if err != nil {
		return err
	}
	return process.Signal(sent)
}

// unpackedBinaries unpacks the binaries once per machine and version. Test binaries of
// several packages start at once, so each unpacks into a scratch directory of its own and
// renames it into place, and the loser drops its copy; a run killed while unpacking leaves
// only its scratch directory, never a broken final one.
func unpackedBinaries() (string, error) {
	final := filepath.Join(stateDirectory(), "livewire-postgres-"+string(version))
	if _, err := os.Stat(filepath.Join(final, "bin", "pg_ctl")); err == nil {
		return final, nil
	}
	defer http.DefaultClient.CloseIdleConnections() // the archive's download would outlive the tests and fail their leak check
	if err := os.MkdirAll(stateDirectory(), 0o700); err != nil {
		return "", err
	}
	scratch, err := os.MkdirTemp(stateDirectory(), "livewire-postgres-unpack-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(scratch) }() // only the renamed copy matters
	port, err := freePort()
	if err != nil {
		return "", err
	}
	unpack := newDatabase(port, filepath.Join(scratch, "binaries"), filepath.Join(scratch, "runtime"))
	if err := unpack.Start(); err != nil { // embedded-postgres unpacks only as part of a start
		return "", err
	}
	if err := unpack.Stop(); err != nil {
		return "", err
	}
	if err := os.Rename(filepath.Join(scratch, "binaries"), final); err != nil {
		if _, statError := os.Stat(filepath.Join(final, "bin", "pg_ctl")); statError != nil {
			return "", err
		}
	}
	return final, nil
}

// stateDirectory is embedded-postgres' own download cache, which the govengers CI restores
// and saves: a CI run then starts from unpacked binaries.
func stateDirectory() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), ".embedded-postgres-go")
	}
	return filepath.Join(home, ".embedded-postgres-go")
}

// instancesDirectory holds the instances of this package's test binaries only, so a sweep
// never looks at another package's running instance. go test runs a package's test binary in
// the package's directory.
func instancesDirectory() string {
	workingDirectory, _ := os.Getwd() // an empty name still gives a usable directory
	digest := sha256.Sum256([]byte(workingDirectory))
	return filepath.Join(stateDirectory(), "livewire-instances", hex.EncodeToString(digest[:8]))
}

func freePort() (int, error) {
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	return port, listener.Close()
}
