// Package livewiretest holds the contract gates an application's test suite runs (D23, D70):
// GeneratedIsCurrent fails when a committed generated file differs from what the registry
// renders now; Parity fails when a read's REST body differs from its socket payload; Stale
// fails when a committed write leaves a live read's subscription behind its REST read; and
// TenantIsolation fails when one tenant reads a row of another (D42).
//
// The gates other than GeneratedIsCurrent drive the application as its clients do: they serve
// Application.Handler, the application's own router with its authentication, its REST routes
// and its socket, on a local test server, and authenticate every request through the caller's
// Authenticate function (an API key header or a session cookie), so what they check is what a
// browser, the generated clients and the CLI get. They fail the test through the testing.TB
// they are given and log nothing else.
package livewiretest
