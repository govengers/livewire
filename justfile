set shell := ["bash", "-euo", "pipefail", "-c"]

gotestsum := "go run gotest.tools/gotestsum@v1.13.0"
govulncheck := "go run golang.org/x/vuln/cmd/govulncheck@v1.9.0"
sqlc := "go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1"
coverage_threshold := "80"
excluded := "/examples/|/test/|/generate/internal/fixture/"
# CI (GitHub sets CI) has bun: the TypeScript parts of the e2e suite fail there instead of skipping
require_typescript := if env_var_or_default("CI", "") != "" { "LIVEWIRE_REQUIRE_TYPESCRIPT=1" } else { "" }

default:
    @just --list

# Vet and lint with the govengers baseline (.golangci.yml, copied from govengers/standards), the example module included
lint:
    go vet ./...
    go vet -tags e2e ./test/e2e/...
    golangci-lint run ./...
    cd examples/todo && go vet ./... && golangci-lint run ./...

# Unit tests with the race detector; fails below the coverage threshold (examples, the e2e suite and the generator's fixture with its generated clients excluded); then the example's tests
test: && test-example
    mkdir -p sandbox
    {{gotestsum}} --format testname --jsonfile sandbox/unit.json -- -race -count=1 -coverprofile=sandbox/unit.raw.cover ./...
    grep -v -E '{{excluded}}' sandbox/unit.raw.cover > sandbox/unit.cover
    go tool cover -func=sandbox/unit.cover > sandbox/unit.func
    awk -v threshold={{coverage_threshold}} '/^total:/ { found = 1; sub("%", "", $3); print "coverage " $3 "%"; if ($3 + 0 < threshold) { print "coverage is below " threshold "%"; exit 1 } } END { if (!found) { print "no coverage total"; exit 1 } }' sandbox/unit.func

# The example application's tests (a module of its own, examples/todo): the contract gates, the generated Go client and CLI against it, the frontend's type check
test-example:
    cd examples/todo && {{require_typescript}} go test -race -count=1 ./...

# The TypeScript gates of the generator (tsc on the generated client, the runtime's bun tests), failing without bun
test-typescript:
    cd generate/runtime && bun install --frozen-lockfile
    LIVEWIRE_REQUIRE_TYPESCRIPT=1 go test -count=1 -run 'TestGeneratedTypeScriptTypeChecks|TestRuntimeUnitTests' ./generate/

# End-to-end: two engines on one embedded Postgres, the TypeScript runtime driven by bun (build tag e2e); without bun its parts skip, except in CI
test-e2e:
    mkdir -p sandbox
    {{require_typescript}} {{gotestsum}} --format testname --jsonfile sandbox/e2e.json -- -tags e2e -count=1 ./test/e2e/...

# Regenerate the example's sqlc queries and its clients (TypeScript, OpenAPI, Go client, CLI)
generate-example:
    cd examples/todo && {{sqlc}} generate && go run ./cmd/generate

# Scan for known vulnerabilities in reachable code, the example module's included
vulncheck:
    {{govulncheck}} ./...
    cd examples/todo && {{govulncheck}} ./...

# Everything CI runs
check: lint test test-e2e vulncheck
