// Package client is the runtime of the Go clients the generator writes (D19, D67): the REST
// requests with the API key (D20), the error body, the Idempotency-Key (D16), the export
// trailer (D60), the stored filters with the recovery of an evicted one (D54), the filter
// document builders (D12), and the one socket that carries every subscription of a Client,
// with reconnect and resubscribe. A generated package `<app>client` wraps it with one typed
// method per declaration; this package knows no declaration and is usable on its own with
// routes and wire parameters.
//
// A Client serves the identity its API key names. A 4401 close (the identity is gone, D7)
// or a socket handshake refused with 401 ends its live side for good: every subscription
// ends with an *EndedError and Subscribe refuses after; its REST requests go on and answer
// what the server answers. Everything in the package is safe for concurrent use.
package client
