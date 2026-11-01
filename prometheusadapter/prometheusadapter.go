package prometheusadapter

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/govengers/livewire"
)

// Options configures New. Registerer is required: the collectors are registered on it, the
// application's own registry, never the default one (LIB-011, MET-002). Namespace is the
// first part of every metric name, the application's name (MET-005); the subsystem is
// "livewire".
type Options struct {
	Registerer prometheus.Registerer
	Namespace  string
}

// Metrics turns the engine's outcomes into Prometheus collectors (design section 2): the
// units of work by kind, socket operation, target and status class with their duration, the
// subscription re-runs, the open sockets and subscriptions, the socket closes by code and
// the queue drops among them, the wait for the engine's query slots, and the failures of the
// background work. Every label value is from a closed set: the outcome kinds and
// operations, the registry's names (the engine reports nothing else, D59), status classes
// and the known close codes. It is safe for concurrent use.
type Metrics struct {
	units              *prometheus.CounterVec
	unitDurations      *prometheus.HistogramVec
	reruns             *prometheus.CounterVec
	rerunDurations     *prometheus.HistogramVec
	sockets            prometheus.Gauge
	subscriptions      prometheus.Gauge
	socketCloses       *prometheus.CounterVec
	queueDrops         prometheus.Counter
	queryWaits         *prometheus.HistogramVec
	backgroundFailures *prometheus.CounterVec
}

// New builds the collectors and registers them on options.Registerer; a registration error
// (a collector registered twice) is returned.
func New(options *Options) (*Metrics, error) {
	if options == nil || options.Registerer == nil {
		return nil, errors.New("prometheusadapter: New needs options with a Registerer")
	}
	namespace := options.Namespace
	metrics := &Metrics{
		units:              counterVector(namespace, "units_total", "Units of work the engine finished, by kind, socket operation, target and status class.", "kind", "operation", "target", "status_class"),
		unitDurations:      histogramVector(namespace, "unit_duration_seconds", "Duration of the units of work, by kind, socket operation and target.", "kind", "operation", "target"),
		reruns:             counterVector(namespace, "reruns_total", "Subscription re-runs after a change, a resync or a tick, by target and status class.", "target", "status_class"),
		rerunDurations:     histogramVector(namespace, "rerun_duration_seconds", "Duration of the subscription re-runs, by target.", "target"),
		sockets:            gauge(namespace, "sockets", "Open sockets."),
		subscriptions:      gauge(namespace, "subscriptions", "Open subscriptions."),
		socketCloses:       counterVector(namespace, "socket_closes_total", "Sockets that ended, by close code.", "code"),
		queueDrops:         counter(namespace, "queue_drops_total", "Sockets closed 1013 because they did not take their queued frames in time."),
		queryWaits:         histogramVector(namespace, "query_wait_seconds", "Wait of the socket work for the engine's query slots, by socket operation (rerun and revalidate for the background work).", "operation"),
		backgroundFailures: counterVector(namespace, "background_failures_total", "Failures of the background work: the change feed, the presence heartbeat, the identity re-validation.", "kind"),
	}
	for _, collector := range metrics.collectors() {
		if err := options.Registerer.Register(collector); err != nil {
			return nil, fmt.Errorf("prometheusadapter: register the livewire collectors: %w", err)
		}
	}
	return metrics, nil
}

const subsystem = "livewire"

func (m *Metrics) collectors() []prometheus.Collector {
	return []prometheus.Collector{
		m.units, m.unitDurations, m.reruns, m.rerunDurations, m.sockets, m.subscriptions,
		m.socketCloses, m.queueDrops, m.queryWaits, m.backgroundFailures,
	}
}

// Observe is a livewire.Observer: pass it as RegistryOptions.Observer, or call it from the
// application's own Observer next to its other reporting (D49).
func (m *Metrics) Observe(_ context.Context, outcome livewire.Outcome) {
	switch outcome.Kind {
	case livewire.OutcomeRequest, livewire.OutcomeSubscribe, livewire.OutcomeCall, livewire.OutcomeBatch, livewire.OutcomeExport:
		m.observeUnit(outcome)
	case livewire.OutcomeRerun:
		m.reruns.WithLabelValues(outcome.Target, statusClass(outcome.Status)).Inc()
		m.rerunDurations.WithLabelValues(outcome.Target).Observe(outcome.Duration.Seconds())
		m.queryWaits.WithLabelValues(string(livewire.OutcomeRerun)).Observe(outcome.QueryWait.Seconds())
	case livewire.OutcomeEnd:
		m.subscriptions.Dec()
	case livewire.OutcomeOpen:
		m.sockets.Inc()
	case livewire.OutcomeClose:
		m.observeClose(outcome.Status)
	case livewire.OutcomeRevalidate:
		m.queryWaits.WithLabelValues(string(livewire.OutcomeRevalidate)).Observe(outcome.QueryWait.Seconds())
		m.countFailure(outcome)
	case livewire.OutcomeFeed, livewire.OutcomeHeartbeat:
		m.countFailure(outcome)
	case livewire.OutcomeAfterCommit:
	}
}

// observeUnit counts a unit of work; a socket frame's own unit (not a call within a batch)
// also reports its wait for the query slots, and a subscribe that answered opens a
// subscription.
func (m *Metrics) observeUnit(outcome livewire.Outcome) {
	operation := string(outcome.Operation)
	m.units.WithLabelValues(string(outcome.Kind), operation, outcome.Target, statusClass(outcome.Status)).Inc()
	m.unitDurations.WithLabelValues(string(outcome.Kind), operation, outcome.Target).Observe(outcome.Duration.Seconds())
	callOfABatch := outcome.Kind == livewire.OutcomeCall && outcome.Operation == livewire.OperationBatch
	if outcome.Operation != "" && !callOfABatch {
		m.queryWaits.WithLabelValues(operation).Observe(outcome.QueryWait.Seconds())
	}
	if outcome.Kind == livewire.OutcomeSubscribe && outcome.Err == nil {
		m.subscriptions.Inc()
	}
}

func (m *Metrics) observeClose(code int) {
	m.sockets.Dec()
	m.socketCloses.WithLabelValues(closeCodeLabel(code)).Inc()
	if code == livewire.CloseTooSlow {
		m.queueDrops.Inc()
	}
}

func (m *Metrics) countFailure(outcome livewire.Outcome) {
	if outcome.Err != nil {
		m.backgroundFailures.WithLabelValues(string(outcome.Kind)).Inc()
	}
}

// statusClass is the label of a status: "2xx" to "5xx", "other" for anything else.
func statusClass(status int) string {
	if status < 200 || status > 599 {
		return "other"
	}
	return strconv.Itoa(status/100) + "xx"
}

// closeCodes are the close codes a socket ends with on the engine's side or a client's
// usual ones; any other code a client sends is "other".
var closeCodes = map[int]bool{1000: true, 1001: true, 1005: true, 1006: true, 1009: true, 1011: true, 1013: true, 4401: true}

func closeCodeLabel(code int) string {
	if !closeCodes[code] {
		return "other"
	}
	return strconv.Itoa(code)
}

func counterVector(namespace, name, help string, labels ...string) *prometheus.CounterVec {
	return prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: namespace, Subsystem: subsystem, Name: name, Help: help}, labels)
}

func histogramVector(namespace, name, help string, labels ...string) *prometheus.HistogramVec {
	return prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: namespace, Subsystem: subsystem, Name: name, Help: help, Buckets: prometheus.DefBuckets}, labels)
}

func gauge(namespace, name, help string) prometheus.Gauge {
	return prometheus.NewGauge(prometheus.GaugeOpts{Namespace: namespace, Subsystem: subsystem, Name: name, Help: help})
}

func counter(namespace, name, help string) prometheus.Counter {
	return prometheus.NewCounter(prometheus.CounterOpts{Namespace: namespace, Subsystem: subsystem, Name: name, Help: help})
}
