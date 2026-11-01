// Package ginadapter serves a livewire engine on gin (D3): RegisterRoutes mounts the REST
// routes of every declaration and of the library itself, SocketHandler is the socket
// endpoint as a gin handler, and every REST outcome is recorded on the request's wide event
// (D4), which the wide-event-logger's gin middleware put into the request. It is the only
// package of the module that imports gin and wide-event-logger; it never logs and never
// emits an event of its own. Everything in it is safe for concurrent use.
package ginadapter
