// Package prometheusadapter exports the live engine's outcomes as Prometheus collectors
// (D3): New registers them on a Registerer the application passes in, never on the default
// registry, and Metrics.Observe is the livewire.Observer that feeds them (D49). It is the
// only package of the module that imports client_golang.
package prometheusadapter
