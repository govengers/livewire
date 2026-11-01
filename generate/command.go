package generate

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/govengers/livewire"
)

var (
	commandNamePattern       = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	environmentPrefixPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	// commandRootFlags are the flags of the root command, which every command inherits.
	commandRootFlags = []string{"base-url", "api-key", "socket-url", "help"}
)

func resolveCommandLine(options *CommandLineOptions) error {
	if options.Directory == "" {
		return nil
	}
	if !commandNamePattern.MatchString(options.Name) {
		return fmt.Errorf("generate: CommandLine.Name %q is no command name of lower-case letters, digits and dashes", options.Name)
	}
	if options.EnvironmentPrefix == "" {
		options.EnvironmentPrefix = strings.ToUpper(strings.ReplaceAll(options.Name, "-", "_"))
	}
	if !environmentPrefixPattern.MatchString(options.EnvironmentPrefix) {
		return fmt.Errorf("generate: CommandLine.EnvironmentPrefix %q is no prefix of upper-case letters, digits and underscores", options.EnvironmentPrefix)
	}
	if options.Package == "" {
		options.Package = filepath.Base(options.Directory)
	}
	return validatePackageName("CommandLine.Package", options.Package)
}

// commandLine renders the command-line client (D19, D20) on github.com/urfave/cli/v3: the
// root command with the connection flags, one command per declaration with one command per
// read kind and its export, one per mutation and upload, the library's batch and filters,
// and subscribe with every live read below it.
type commandLine struct {
	api       *model
	options   CommandLineOptions
	functions strings.Builder
	written   map[string]bool
}

func renderCommandLine(api *model, options CommandLineOptions) ([]byte, error) {
	renderer := &commandLine{api: api, options: options, written: map[string]bool{}}
	commands, err := renderer.rootCommands()
	if err != nil {
		return nil, err
	}
	replacer := strings.NewReplacer("{package}", options.Package, "{name}", options.Name, "{prefix}", options.EnvironmentPrefix, "{base}", basePathOrRoot(api.basePath))
	source := goHeader + replacer.Replace(commandLinePreamble) + commands + replacer.Replace(commandLineHelpers) + renderer.functions.String()
	return formatGo("command.go", source)
}

// commandGroup is the reads and the export of one declaration.
type commandGroup struct {
	name   string
	reads  []*readModel
	export *exportModel
}

func (c *commandLine) groups() []*commandGroup {
	var groups []*commandGroup
	byName := map[string]*commandGroup{}
	for _, read := range c.api.reads {
		group := byName[read.declaration]
		if group == nil {
			group = &commandGroup{name: read.declaration}
			byName[read.declaration] = group
			groups = append(groups, group)
		}
		group.reads = append(group.reads, read)
	}
	for _, export := range c.api.exports {
		byName[export.declaration].export = export
	}
	return groups
}

func readCommandName(read *readModel) string {
	switch {
	case read.kind != livewire.ReadMain:
		return string(read.kind)
	case read.list:
		return "list"
	default:
		return "get"
	}
}

func (c *commandLine) rootCommands() (string, error) {
	names := newCommandNames("subscribe", "batch", "filters", "help")
	var root, live strings.Builder
	for _, group := range c.groups() {
		if err := names.add(kebabCase(group.name), "the declaration "+group.name); err != nil {
			return "", err
		}
		var reads, subscriptions strings.Builder
		for _, read := range group.reads {
			parameters, err := c.parameterList(read.queryType, wireParameters(read.parameters, read.reserved), nil)
			if err != nil {
				return "", fmt.Errorf("%w (the read %s)", err, read.targetName())
			}
			route := c.api.basePath + read.path
			fmt.Fprintf(&reads, "\t\t\t\treadCommand(%q, %q, %q, %s),\n", readCommandName(read), "GET "+route+", the socket target "+read.targetName(), read.path, parameters)
			if read.live {
				fmt.Fprintf(&subscriptions, "\t\t\t\t\tsubscribeCommand(%q, %q, %q, %s),\n", readCommandName(read), "subscribes to "+read.targetName()+" (GET "+route+")", read.targetName(), parameters)
			}
		}
		if group.export != nil {
			parameters, err := c.parameterList(group.export.queryType, wireParameters(nil, group.export.reserved), []string{"output"})
			if err != nil {
				return "", err
			}
			fmt.Fprintf(&reads, "\t\t\t\texportCommand(\"export\", %q, %q, %s),\n", "GET "+c.api.basePath+group.export.path+" as CSV", group.export.path, parameters)
		}
		fmt.Fprintf(&root, "\t\t\t{Name: %q, Usage: %q, Commands: []*cli.Command{\n%s\t\t\t}},\n", kebabCase(group.name), "the reads of "+group.name, reads.String())
		if subscriptions.Len() > 0 {
			fmt.Fprintf(&live, "\t\t\t\t{Name: %q, Usage: %q, Commands: []*cli.Command{\n%s\t\t\t\t}},\n", kebabCase(group.name), "the live reads of "+group.name, subscriptions.String())
		}
	}
	if err := c.writes(&root, names); err != nil {
		return "", err
	}
	fmt.Fprintf(&root, "\t\t\tbatchCommand(),\n\t\t\tfiltersCommand(%s),\n", goStrings(c.filterTargets()))
	fmt.Fprintf(&root, "\t\t\t{Name: \"subscribe\", Usage: \"streams a live read as NDJSON, one push per line, until interrupted\", Commands: []*cli.Command{\n%s\t\t\t}},\n", live.String())
	return root.String() + "\t\t},\n\t})\n}\n", nil
}

func (c *commandLine) writes(root *strings.Builder, names *commandNames) error {
	for _, mutation := range c.api.mutations {
		if err := names.add(kebabCase(mutation.name), "the mutation "+mutation.name); err != nil {
			return err
		}
		extra := []string{"idempotency-key"}
		if mutation.body != nil {
			extra = append(extra, "body")
		}
		parameters, err := c.parameterList(mutation.parametersType, wireParameters(mutation.parameters, reservedModel{}), extra)
		if err != nil {
			return fmt.Errorf("%w (the mutation %s)", err, mutation.name)
		}
		usage := fmt.Sprintf("%s %s%s, answering %d", mutation.method, c.api.basePath, mutation.path, mutation.status)
		fmt.Fprintf(root, "\t\t\tcallCommand(%q, %q, %q, %q, %t, %s),\n", kebabCase(mutation.name), usage, mutation.method, mutation.path, mutation.body != nil, parameters)
	}
	for _, upload := range c.api.uploads {
		if err := names.add(kebabCase(upload.name), "the upload "+upload.name); err != nil {
			return err
		}
		parameters, err := c.parameterList(upload.parametersType, wireParameters(upload.parameters, reservedModel{}), []string{"file", "content-type", "idempotency-key"})
		if err != nil {
			return fmt.Errorf("%w (the upload %s)", err, upload.name)
		}
		usage := fmt.Sprintf("POST %s%s: a raw file of at most %d bytes, answering %d", c.api.basePath, upload.path, upload.maximumBytes, upload.status)
		fmt.Fprintf(root, "\t\t\tuploadCommand(%q, %q, %q, %s, %s),\n", kebabCase(upload.name), usage, upload.path, goStrings(upload.contentTypes), parameters)
	}
	return nil
}

// filterTargets are the targets a stored filter may name (D13, D56).
func (c *commandLine) filterTargets() []string {
	var targets []string
	for _, read := range c.api.reads {
		if read.reserved.filters != nil && read.kind != livewire.ReadDistinct {
			targets = append(targets, read.targetName())
		}
	}
	return targets
}

// parameterList writes the function that lists a parameter set once and answers its call;
// the flags must not collide with each other, the root's or the command's own (extra).
func (c *commandLine) parameterList(typeName string, parameters []wireParameter, extra []string) (string, error) {
	flags := newCommandNames(slices.Concat(commandRootFlags, extra)...)
	var entries strings.Builder
	for _, parameter := range parameters {
		flag := kebabCase(parameter.name)
		if err := flags.add(flag, "the parameter "+parameter.name); err != nil {
			return "", err
		}
		entry, usage := fmt.Sprintf("flag: %q, wire: %q, kind: %s", flag, parameter.name, commandKinds[parameter.kind]), parameter.usage
		if parameter.required {
			entry, usage = entry+", required: true", usage+"; required"
		}
		if len(parameter.values) > 0 {
			entry += ", values: " + goStrings(parameter.values)
		}
		fmt.Fprintf(&entries, "\t\t{%s, usage: %q},\n", entry, usage)
	}
	if len(parameters) == 0 {
		return "nil", nil
	}
	function := lowerFirst(typeName)
	if !c.written[function] {
		c.written[function] = true
		fmt.Fprintf(&c.functions, "\nfunc %s() []parameter {\n\treturn []parameter{\n%s\t}\n}\n", function, entries.String())
	}
	return function + "()", nil
}

var commandKinds = map[valueKind]string{
	valueString: "kindString", valueInteger: "kindInteger", valueBoolean: "kindBoolean", valueUUID: "kindUUID",
	valueTime: "kindTime", valueList: "kindList", valueFilter: "kindDocument",
}

func goStrings(values []string) string {
	if len(values) == 0 {
		return "nil"
	}
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, strconv.Quote(value))
	}
	return "[]string{" + strings.Join(quoted, ", ") + "}"
}

// commandNames keeps the names of one command level (or one command's flags).
type commandNames struct {
	taken map[string]string
}

func newCommandNames(fixed ...string) *commandNames {
	names := &commandNames{taken: map[string]string{}}
	for _, name := range fixed {
		names.taken[name] = "the command line itself"
	}
	return names
}

func (n *commandNames) add(name, owner string) error {
	if earlier, ok := n.taken[name]; ok {
		return fmt.Errorf("generate: %s and %s are both %q on the command line; rename one", earlier, owner, name)
	}
	n.taken[name] = owner
	return nil
}

// commandLinePreamble is the package up to the root command's subcommands.
const commandLinePreamble = `// Package {package} is the command-line client {name} of the API at {base} (D19, D20), on
// github.com/urfave/cli/v3, generated from its livewire registry: one command per declaration
// and read kind, the parameters as flags, a body as --body or on stdin, the answers as JSON on
// stdout and the diagnostics on stderr. The API's URL with its base path comes from --base-url
// or {prefix}_BASE_URL, the API key from --api-key or {prefix}_API_KEY. A binary's main is
//
//	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
//	defer stop()
//	os.Exit({package}.Run(ctx, os.Args, os.Stdin, os.Stdout, os.Stderr))
package {package}

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/urfave/cli/v3"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/client"
)

// The exit codes of Run.
const (
	ExitSuccess = 0 // the command did what it says
	ExitFailure = 1 // the server failed (5xx) or could not be reached
	ExitUsage   = 2 // the command line is wrong
	ExitRefused = 3 // the server refused the request (4xx), or the API key's identity is gone
)

// Run runs the command line arguments (as os.Args, the program's name first): a body is read
// from input when --body is absent, every answer is written to output as JSON and every
// diagnostic to diagnostics. It answers the exit code.
func Run(ctx context.Context, arguments []string, input io.Reader, output, diagnostics io.Writer) int {
	root := Command()
	locked := &lockedWriter{writer: diagnostics}
	defer locked.close() // nothing reaches diagnostics after Run returned
	root.Reader, root.Writer, root.ErrWriter = input, output, locked
	root.ExitErrHandler = func(context.Context, *cli.Command, error) {}
	err := root.Run(ctx, arguments)
	if err == nil {
		return ExitSuccess
	}
	report(locked, err)
	return exitCode(err)
}

// lockedWriter serializes the diagnostics, which the socket loop writes from its own
// goroutine (a failed connect), and drops what comes after Run returned.
type lockedWriter struct {
	mutex  sync.Mutex
	writer io.Writer
	closed bool
}

func (w *lockedWriter) Write(data []byte) (int, error) {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	if w.closed {
		return len(data), nil
	}
	return w.writer.Write(data)
}

func (w *lockedWriter) close() {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	w.closed = true
}

// Command is the root command {name}; Run runs it with the given streams. A command line
// mistake is returned without urfave's help text, which would land on stdout: the
// diagnostic goes to stderr, explicit --help to stdout.
func Command() *cli.Command {
	return quietUsage(&cli.Command{
		Name:  "{name}",
		Usage: "the API at {base}",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "base-url", Usage: "the API's URL with its base path {base}", Sources: cli.EnvVars("{prefix}_BASE_URL")},
			&cli.StringFlag{Name: "api-key", Usage: "the API key (D20)", Sources: cli.EnvVars("{prefix}_API_KEY")},
			&cli.StringFlag{Name: "socket-url", Usage: "the socket's URL (default: the base URL's /socket)", Sources: cli.EnvVars("{prefix}_SOCKET_URL")},
		},
		Commands: []*cli.Command{
`

// commandLineHelpers are the commands and helpers every generated CLI shares.
const commandLineHelpers = `
// parameter is one parameter of a command: its flag, its wire name, the kind of its value,
// whether it is required and the values it may take.
type parameter struct {
	flag     string
	wire     string
	kind     kind
	required bool
	values   []string
	usage    string
}

type kind int

const (
	kindString kind = iota
	kindInteger
	kindBoolean
	kindUUID
	kindTime
	kindList
	kindDocument
)

// usageError is a mistake of the command line; failure one of the server or the way there.
type (
	usageError struct{ err error }
	failure    struct{ err error }
)

func (e *usageError) Error() string { return e.err.Error() }
func (e *failure) Error() string    { return e.err.Error() }
func (e *failure) Unwrap() error    { return e.err }

func usagef(format string, arguments ...any) error {
	return &usageError{err: fmt.Errorf(format, arguments...)}
}

// failed keeps a refusal and marks anything else as a failure.
func failed(err error) error {
	var refusal *client.Error
	if errors.As(err, &refusal) {
		return err
	}
	return &failure{err: err}
}

func exitCode(err error) int {
	var refusal *client.Error
	var ended *client.EndedError
	var mistake *usageError
	var failing *failure
	switch {
	case errors.As(err, &refusal):
		if refusal.Status >= 400 && refusal.Status < 500 {
			return ExitRefused
		}
		return ExitFailure
	case errors.As(err, &ended):
		return ExitRefused
	case errors.As(err, &mistake):
		return ExitUsage
	case errors.As(err, &failing):
		return ExitFailure
	default:
		return ExitUsage // the command line parser's own
	}
}

// quietUsage makes every command of the tree return its usage errors instead of printing
// them with its help.
func quietUsage(command *cli.Command) *cli.Command {
	command.OnUsageError = func(_ context.Context, _ *cli.Command, err error, _ bool) error { return err }
	for _, subcommand := range command.Commands {
		quietUsage(subcommand)
	}
	return command
}

func report(diagnostics io.Writer, err error) {
	_, _ = fmt.Fprintf(diagnostics, "{name}: %v\n", err)
	if exitCode(err) == ExitUsage {
		_, _ = fmt.Fprintln(diagnostics, "{name}: see --help")
	}
	var refusal *client.Error
	if !errors.As(err, &refusal) {
		return
	}
	names := make([]string, 0, len(refusal.Fields))
	for name := range refusal.Fields {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		_, _ = fmt.Fprintf(diagnostics, "  %s: %s\n", name, refusal.Fields[name])
	}
}

func flags(parameters []parameter, extra ...cli.Flag) []cli.Flag {
	listed := make([]cli.Flag, 0, len(parameters)+len(extra))
	for _, current := range parameters {
		listed = append(listed, flagOf(current))
	}
	return append(listed, extra...)
}

func flagOf(current parameter) cli.Flag {
	switch current.kind {
	case kindInteger:
		return &cli.Int64Flag{Name: current.flag, Usage: current.usage, Required: current.required}
	case kindBoolean:
		return &cli.BoolFlag{Name: current.flag, Usage: current.usage, Required: current.required}
	case kindList:
		return &cli.StringSliceFlag{Name: current.flag, Usage: current.usage, Required: current.required, Validator: func(values []string) error {
			for _, value := range values {
				if err := checkValue(current, value); err != nil {
					return err
				}
			}
			return nil
		}}
	default:
		return &cli.StringFlag{Name: current.flag, Usage: current.usage, Required: current.required, Validator: func(value string) error {
			return checkValue(current, value)
		}}
	}
}

func checkValue(current parameter, value string) error {
	switch {
	case current.required && value == "":
		return fmt.Errorf("--%s must not be empty", current.flag)
	case len(current.values) > 0 && !slices.Contains(current.values, value):
		return fmt.Errorf("--%s must be one of %s", current.flag, strings.Join(current.values, ", "))
	case current.kind == kindUUID:
		if _, err := uuid.Parse(value); err != nil {
			return fmt.Errorf("--%s must be a uuid", current.flag)
		}
	case current.kind == kindTime:
		if _, err := time.Parse(time.RFC3339, value); err != nil {
			return fmt.Errorf("--%s must be an RFC 3339 time", current.flag)
		}
	case current.kind == kindDocument:
		if !json.Valid([]byte(value)) {
			return fmt.Errorf("--%s must be a JSON document", current.flag)
		}
	}
	return nil
}

// wireOf is the wire parameters of the flags the command line set.
func wireOf(command *cli.Command, parameters []parameter) client.Parameters {
	wire := client.Parameters{}
	for _, current := range parameters {
		if !command.IsSet(current.flag) {
			continue
		}
		switch current.kind {
		case kindInteger:
			wire[current.wire] = strconv.FormatInt(command.Int64(current.flag), 10)
		case kindBoolean:
			wire[current.wire] = strconv.FormatBool(command.Bool(current.flag))
		case kindList:
			wire[current.wire] = strings.Join(command.StringSlice(current.flag), ",")
		default:
			wire[current.wire] = command.String(current.flag)
		}
	}
	return wire
}

// runtimeOf builds the client of the root's connection flags.
func runtimeOf(command *cli.Command) (*client.Client, error) {
	switch {
	case command.String("base-url") == "":
		return nil, usagef("the API's URL is required: --base-url or {prefix}_BASE_URL")
	case command.String("api-key") == "":
		return nil, usagef("the API key is required: --api-key or {prefix}_API_KEY")
	}
	diagnostics := command.Root().ErrWriter
	runtime, err := client.New(&client.Options{
		BaseURL: command.String("base-url"), APIKey: command.String("api-key"), SocketURL: command.String("socket-url"),
		ConnectFailed: func(err error) { _, _ = fmt.Fprintf(diagnostics, "{name}: %v; reconnecting\n", err) },
	})
	if err != nil {
		return nil, &usageError{err: err}
	}
	return runtime, nil
}

func writeJSON(command *cli.Command, document json.RawMessage) error {
	if len(document) == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(command.Root().Writer, "%s\n", document); err != nil {
		return &failure{err: err}
	}
	return nil
}

// bodyOf is the JSON of --body, or of the input when it is absent.
func bodyOf(command *cli.Command) (json.RawMessage, error) {
	document := []byte(command.String("body"))
	if !command.IsSet("body") {
		read, err := io.ReadAll(command.Root().Reader)
		if err != nil {
			return nil, &failure{err: err}
		}
		document = read
	}
	if !json.Valid(document) {
		return nil, usagef("the body is no JSON document: give it as --body or on stdin")
	}
	return document, nil
}

func bodyFlag() cli.Flag {
	return &cli.StringFlag{Name: "body", Usage: "the JSON body (default: stdin)"}
}

func keyFlag() cli.Flag {
	return &cli.StringFlag{Name: "idempotency-key", Usage: "the Idempotency-Key, a UUID (D16; default: a fresh one, shown when the call fails): repeat it to retry a call safely", Validator: func(value string) error {
		if _, err := uuid.Parse(value); err != nil {
			return errors.New("--idempotency-key must be a UUID")
		}
		return nil
	}}
}

// keyOf is the call's Idempotency-Key: the flag's, or a fresh one.
func keyOf(command *cli.Command) string {
	if command.IsSet("idempotency-key") {
		return command.String("idempotency-key")
	}
	return client.NewIdempotencyKey()
}

// failedCall reports a failed call with the key it went out under, so it can be repeated
// safely, unless the command line named the key itself.
func failedCall(command *cli.Command, key string, err error) error {
	if !command.IsSet("idempotency-key") {
		_, _ = fmt.Fprintf(command.Root().ErrWriter, "{name}: the call went out under the Idempotency-Key %s; repeat it with --idempotency-key %s\n", key, key)
	}
	return failed(err)
}

func readCommand(name, usage, path string, parameters []parameter) *cli.Command {
	return &cli.Command{Name: name, Usage: usage, Flags: flags(parameters), Action: func(ctx context.Context, command *cli.Command) error {
		runtime, err := runtimeOf(command)
		if err != nil {
			return err
		}
		var data json.RawMessage
		if err := runtime.Read(ctx, path, wireOf(command, parameters), &data); err != nil {
			return failed(err)
		}
		return writeJSON(command, data)
	}}
}

// pushLine is one line a subscription writes.
type pushLine struct {
	Kind livewire.PushKind ` + "`json:\"kind\"`" + `
	Data json.RawMessage   ` + "`json:\"data\"`" + `
}

func subscribeCommand(name, usage, target string, parameters []parameter) *cli.Command {
	return &cli.Command{Name: name, Usage: usage, Flags: flags(parameters), Action: func(ctx context.Context, command *cli.Command) error {
		runtime, err := runtimeOf(command)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithCancel(ctx) // the subscription ends with the command, however it returns
		defer cancel()
		pushes, err := client.Subscribe[json.RawMessage](ctx, runtime, target, wireOf(command, parameters))
		if err != nil {
			return failed(err)
		}
		encoder := json.NewEncoder(command.Root().Writer)
		for push := range pushes {
			if push.Kind == livewire.PushError {
				return failed(push.Err)
			}
			if err := encoder.Encode(pushLine{Kind: push.Kind, Data: push.Data}); err != nil {
				return &failure{err: err}
			}
		}
		return nil // interrupted
	}}
}

func exportCommand(name, usage, path string, parameters []parameter) *cli.Command {
	output := &cli.StringFlag{Name: "output", Usage: "the file to write (default: stdout); a cut export removes it", TakesFile: true}
	return &cli.Command{Name: name, Usage: usage, Flags: flags(parameters, output), Action: func(ctx context.Context, command *cli.Command) error {
		runtime, err := runtimeOf(command)
		if err != nil {
			return err
		}
		outputPath := command.String("output")
		if outputPath == "" {
			rows, err := runtime.Export(ctx, path, wireOf(command, parameters), command.Root().Writer)
			if err != nil {
				return failed(err)
			}
			_, _ = fmt.Fprintf(command.Root().ErrWriter, "%d rows\n", rows)
			return nil
		}
		created, err := os.Create(outputPath)
		if err != nil {
			return usagef("--output: %w", err)
		}
		rows, err := runtime.Export(ctx, path, wireOf(command, parameters), created)
		if err := finishOutput(created, outputPath, err); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(command.Root().ErrWriter, "%d rows\n", rows)
		return nil
	}}
}

func callCommand(name, usage, method, path string, body bool, parameters []parameter) *cli.Command {
	extra := []cli.Flag{keyFlag()}
	if body {
		extra = append(extra, bodyFlag())
	}
	return &cli.Command{Name: name, Usage: usage, Flags: flags(parameters, extra...), Action: func(ctx context.Context, command *cli.Command) error {
		runtime, err := runtimeOf(command)
		if err != nil {
			return err
		}
		call := &client.Call{Method: method, Path: path, Parameters: wireOf(command, parameters), IdempotencyKey: keyOf(command)}
		if body {
			if call.Body, err = bodyOf(command); err != nil {
				return err
			}
		}
		var response json.RawMessage
		if err := runtime.Call(ctx, call, &response); err != nil {
			return failedCall(command, call.IdempotencyKey, err)
		}
		return writeJSON(command, response)
	}}
}

func uploadCommand(name, usage, path string, contentTypes []string, parameters []parameter) *cli.Command {
	file := &cli.StringFlag{Name: "file", Usage: "the file to upload, sent raw", Required: true, TakesFile: true}
	contentType := &cli.StringFlag{Name: "content-type", Usage: "the file's type, one of " + strings.Join(contentTypes, ", "), Validator: func(value string) error {
		if !slices.Contains(contentTypes, value) {
			return fmt.Errorf("--content-type must be one of %s", strings.Join(contentTypes, ", "))
		}
		return nil
	}}
	return &cli.Command{Name: name, Usage: usage, Flags: flags(parameters, file, contentType, keyFlag()), Action: func(ctx context.Context, command *cli.Command) error {
		runtime, err := runtimeOf(command)
		if err != nil {
			return err
		}
		declared := command.String("content-type")
		if declared == "" && len(contentTypes) == 1 {
			declared = contentTypes[0]
		}
		if declared == "" {
			return usagef("--content-type is required: one of %s", strings.Join(contentTypes, ", "))
		}
		opened, err := os.Open(command.String("file"))
		if err != nil {
			return usagef("--file: %w", err)
		}
		defer func() { _ = opened.Close() }()
		information, err := opened.Stat()
		if err != nil {
			return usagef("--file: %w", err)
		}
		upload := &client.Upload{Path: path, Parameters: wireOf(command, parameters), IdempotencyKey: keyOf(command),
			File: livewire.File{Body: opened, Size: information.Size(), ContentType: declared}}
		var response json.RawMessage
		if err := runtime.Upload(ctx, upload, &response); err != nil {
			return failedCall(command, upload.IdempotencyKey, err)
		}
		return writeJSON(command, response)
	}}
}

func batchCommand() *cli.Command {
	return &cli.Command{Name: "batch", Usage: "POST {base}/batch: several mutations in one transaction (D33, D47), the body a JSON array of {target, params, body}", Flags: []cli.Flag{bodyFlag(), keyFlag()}, Action: func(ctx context.Context, command *cli.Command) error {
		runtime, err := runtimeOf(command)
		if err != nil {
			return err
		}
		document, err := bodyOf(command)
		if err != nil {
			return err
		}
		var calls []livewire.BatchCall
		if err := json.Unmarshal(document, &calls); err != nil {
			return usagef("the body is no array of batch calls: %w", err)
		}
		key := keyOf(command)
		results, err := runtime.Batch(ctx, calls, &client.CallOptions{IdempotencyKey: key})
		if err != nil {
			return failedCall(command, key, err)
		}
		encoded, err := json.Marshal(results)
		if err != nil {
			return &failure{err: err}
		}
		return writeJSON(command, encoded)
	}}
}

func filtersCommand(targets []string) *cli.Command {
	resource := func(required bool) cli.Flag {
		return &cli.StringFlag{Name: "resource", Usage: "the target, one of " + strings.Join(targets, ", "), Required: required, Validator: func(value string) error {
			if !slices.Contains(targets, value) {
				return fmt.Errorf("--resource must be one of %s", strings.Join(targets, ", "))
			}
			return nil
		}}
	}
	save := &cli.Command{Name: "save", Usage: "stores a filter (POST {base}/filters, D13)", Flags: []cli.Flag{
		resource(true),
		&cli.StringFlag{Name: "filter", Usage: "the filter document (JSON)", Required: true},
		&cli.StringFlag{Name: "name", Usage: "its name (default: unnamed, swept after 30 days unused)"},
		&cli.StringFlag{Name: "context", Usage: "the application's context of the dedup key"},
	}, Action: func(ctx context.Context, command *cli.Command) error {
		runtime, err := runtimeOf(command)
		if err != nil {
			return err
		}
		if !json.Valid([]byte(command.String("filter"))) {
			return usagef("--filter must be a JSON document")
		}
		stored, err := runtime.SaveFilter(ctx, &livewire.FilterRequest{Resource: command.String("resource"), Name: command.String("name"), Context: command.String("context"), Filter: json.RawMessage(command.String("filter"))})
		if err != nil {
			return failed(err)
		}
		return writeJSONOf(command, stored)
	}}
	list := &cli.Command{Name: "list", Usage: "lists the named filters (GET {base}/filters)", Flags: []cli.Flag{resource(false)}, Action: func(ctx context.Context, command *cli.Command) error {
		runtime, err := runtimeOf(command)
		if err != nil {
			return err
		}
		listed, err := runtime.ListFilters(ctx, command.String("resource"))
		if err != nil {
			return failed(err)
		}
		return writeJSONOf(command, listed)
	}}
	remove := &cli.Command{Name: "delete", Usage: "removes a filter (DELETE {base}/filters/{id})", Flags: []cli.Flag{&cli.StringFlag{Name: "id", Usage: "the filter's id", Required: true, Validator: func(value string) error {
		if value == "" {
			return errors.New("--id must not be empty")
		}
		return nil
	}}}, Action: func(ctx context.Context, command *cli.Command) error {
		runtime, err := runtimeOf(command)
		if err != nil {
			return err
		}
		return failedOrNil(runtime.DeleteFilter(ctx, command.String("id")))
	}}
	return &cli.Command{Name: "filters", Usage: "the stored filters (D13)", Commands: []*cli.Command{save, list, remove}}
}

// finishOutput closes the file an export wrote and removes it when the export failed or the
// file did not close: a file that did not close is not written.
func finishOutput(file io.Closer, path string, err error) error {
	if closeErr := file.Close(); err == nil && closeErr != nil {
		err = &failure{err: fmt.Errorf("--output: %w", closeErr)}
	}
	if err != nil {
		_ = os.Remove(path)
		return failed(err)
	}
	return nil
}

func failedOrNil(err error) error {
	if err == nil {
		return nil
	}
	return failed(err)
}

func writeJSONOf(command *cli.Command, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return &failure{err: err}
	}
	return writeJSON(command, encoded)
}
`
