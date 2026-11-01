//go:build e2e

// Package e2e runs two live engines on one embedded PostgreSQL, each with its own pool,
// Listener and gin server, as two replicas of one application (D24), and drives them with the
// Go client runtime and the generated TypeScript runtime under bun: a write on one engine
// re-runs a subscription on the other, an idempotency key replays across them, the Go client
// and the TypeScript runtime recover a filter the FilterStore evicted (D54), a re-created item
// starts a new history (D56), a batch names each call in the history (D58), presence crosses
// engines, every call leaves an audit row, the export streams with its trailer, the Go client
// reconnects across a deploy, and the TypeScript pair model survives a re-login (D66). Without
// bun the TypeScript run skips, unless LIVEWIRE_REQUIRE_TYPESCRIPT=1 (just test-e2e in CI).
package e2e
