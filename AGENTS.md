# Agent instructions

govengers/livewire: the API contract library of the govengers stack. Module `github.com/govengers/livewire`, Go 1.26, govengers/standards library profile (`.govengers.yml`).

- Read `docs/design.md` first; every decision there (D1-D41) is binding. A change to a decision gets a new number in the table, never a rewrite.
- Work packages WP0-WP10 (design section 10) land on `feat/v1`, one branch and one pull request each, `just check` green, dual review before merge.
- Names are spelled out (NAM-001); no logging (LIB-003); constructors `New...(options *...Options) (*X, error)` that validate and copy (DES-001); core packages on `net/http` and `context`, frameworks in `<name>adapter` packages (LIB-007).
- Generated TypeScript in `generate/runtime` is tested under bun; keep `exactOptionalPropertyTypes` passing.
- Commit messages carry no AI attribution.
