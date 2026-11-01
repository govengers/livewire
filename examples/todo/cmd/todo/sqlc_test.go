package main

import (
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

// sqlcModule is the sqlc the queries are generated and vetted with.
const sqlcModule = "github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1"

// TestTheQueriesPassSqlcVet runs sqlc vet, its rules and every query's prepare, against a
// fresh database the application's own migrator migrated (DAT-016). It compiles sqlc, so it
// runs only with LIVEWIRE_SQLC_VET=1: just db-check-example sets it.
func TestTheQueriesPassSqlcVet(t *testing.T) {
	if os.Getenv("LIVEWIRE_SQLC_VET") != "1" {
		t.Skip("sqlc vet runs with LIVEWIRE_SQLC_VET=1 (just db-check-example)")
	}
	pool := newPool(t)
	command := exec.CommandContext(t.Context(), "go", "run", sqlcModule, "vet")
	command.Dir = "../.."
	command.Env = append(os.Environ(), "SQLC_DATABASE_URL="+pool.Config().ConnString())
	output, err := command.CombinedOutput()
	require.NoError(t, err, "sqlc vet:\n%s", output)
}
