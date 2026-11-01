package prometheusadapter_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/prometheusadapter"
)

// sample is the value of the metric name with exactly these labels in the registry: a
// counter's or gauge's value, a histogram's observation count.
func sample(t *testing.T, registry *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := registry.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			if matches(metric, labels) {
				return value(metric)
			}
		}
	}
	return 0
}

func matches(metric *dto.Metric, labels map[string]string) bool {
	if len(metric.GetLabel()) != len(labels) {
		return false
	}
	for _, label := range metric.GetLabel() {
		if labels[label.GetName()] != label.GetValue() {
			return false
		}
	}
	return true
}

func value(metric *dto.Metric) float64 {
	switch {
	case metric.GetCounter() != nil:
		return metric.GetCounter().GetValue()
	case metric.GetGauge() != nil:
		return metric.GetGauge().GetValue()
	default:
		return float64(metric.GetHistogram().GetSampleCount())
	}
}

func TestOutcomesMoveTheCollectors(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics, err := prometheusadapter.New(&prometheusadapter.Options{Registerer: registry, Namespace: "shop"})
	require.NoError(t, err)
	observe := func(outcome livewire.Outcome) { metrics.Observe(context.Background(), outcome) }
	refused := errors.New("forbidden")

	observe(livewire.Outcome{Kind: livewire.OutcomeOpen, Status: http.StatusOK})
	observe(livewire.Outcome{Kind: livewire.OutcomeOpen, Status: http.StatusOK})
	observe(livewire.Outcome{Kind: livewire.OutcomeSubscribe, Operation: livewire.OperationSubscribe, Target: "parts", Status: http.StatusOK, QueryWait: time.Millisecond})
	observe(livewire.Outcome{Kind: livewire.OutcomeSubscribe, Operation: livewire.OperationSubscribe, Target: "parts.item", Status: http.StatusForbidden, Err: refused})
	observe(livewire.Outcome{Kind: livewire.OutcomeRerun, Target: "parts", Status: http.StatusOK, Duration: time.Millisecond})
	observe(livewire.Outcome{Kind: livewire.OutcomeRerun, Target: "parts", Status: http.StatusInternalServerError})
	observe(livewire.Outcome{Kind: livewire.OutcomeEnd, Operation: livewire.OperationSubscribe, Target: "parts"})
	observe(livewire.Outcome{Kind: livewire.OutcomeCall, Operation: livewire.OperationBatch, Target: "createPart", Status: http.StatusCreated})
	observe(livewire.Outcome{Kind: livewire.OutcomeBatch, Operation: livewire.OperationBatch, Target: "batch", Status: http.StatusOK})
	observe(livewire.Outcome{Kind: livewire.OutcomeRequest, Target: "parts", Status: http.StatusOK})
	observe(livewire.Outcome{Kind: livewire.OutcomeExport, Target: "parts", Status: http.StatusOK, Rows: 3})
	observe(livewire.Outcome{Kind: livewire.OutcomeAfterCommit, Target: "createPart", Status: http.StatusCreated})
	observe(livewire.Outcome{Kind: livewire.OutcomeClose, Status: livewire.CloseTooSlow})
	observe(livewire.Outcome{Kind: livewire.OutcomeClose, Status: 3000})
	observe(livewire.Outcome{Kind: livewire.OutcomeFeed, Status: http.StatusInternalServerError, Err: errors.New("connection lost")})
	observe(livewire.Outcome{Kind: livewire.OutcomeHeartbeat, Status: http.StatusOK})
	observe(livewire.Outcome{Kind: livewire.OutcomeRevalidate, Status: http.StatusOK, QueryWait: time.Millisecond})

	testCases := []struct {
		name   string
		labels map[string]string
		value  float64
	}{
		{"shop_livewire_units_total", map[string]string{"kind": "subscribe", "operation": "subscribe", "target": "parts", "status_class": "2xx"}, 1},
		{"shop_livewire_units_total", map[string]string{"kind": "subscribe", "operation": "subscribe", "target": "parts.item", "status_class": "4xx"}, 1},
		{"shop_livewire_units_total", map[string]string{"kind": "request", "operation": "", "target": "parts", "status_class": "2xx"}, 1},
		{"shop_livewire_units_total", map[string]string{"kind": "export", "operation": "", "target": "parts", "status_class": "2xx"}, 1},
		{"shop_livewire_unit_duration_seconds", map[string]string{"kind": "batch", "operation": "batch", "target": "batch"}, 1},
		{"shop_livewire_reruns_total", map[string]string{"target": "parts", "status_class": "5xx"}, 1},
		{"shop_livewire_rerun_duration_seconds", map[string]string{"target": "parts"}, 2},
		{"shop_livewire_sockets", nil, 0},
		{"shop_livewire_subscriptions", nil, 0},
		{"shop_livewire_socket_closes_total", map[string]string{"code": "1013"}, 1},
		{"shop_livewire_socket_closes_total", map[string]string{"code": "other"}, 1},
		{"shop_livewire_queue_drops_total", nil, 1},
		{"shop_livewire_query_wait_seconds", map[string]string{"operation": "subscribe"}, 2},
		{"shop_livewire_query_wait_seconds", map[string]string{"operation": "batch"}, 1},
		{"shop_livewire_query_wait_seconds", map[string]string{"operation": "rerun"}, 2},
		{"shop_livewire_query_wait_seconds", map[string]string{"operation": "revalidate"}, 1},
		{"shop_livewire_background_failures_total", map[string]string{"kind": "feed"}, 1},
		{"shop_livewire_background_failures_total", map[string]string{"kind": "heartbeat"}, 0},
	}
	for _, testCase := range testCases {
		require.InDelta(t, testCase.value, sample(t, registry, testCase.name, testCase.labels), 0, "%s %v", testCase.name, testCase.labels)
	}
	require.Equal(t, 6, testutil.CollectAndCount(registry, "shop_livewire_units_total"), "an after-commit is no unit of work")

	problems, err := testutil.GatherAndLint(registry)
	require.NoError(t, err)
	require.Empty(t, problems)
}

func TestNewValidatesAndReturnsRegistrationErrors(t *testing.T) {
	_, err := prometheusadapter.New(nil)
	require.Error(t, err)
	_, err = prometheusadapter.New(&prometheusadapter.Options{})
	require.Error(t, err)
	registry := prometheus.NewRegistry()
	_, err = prometheusadapter.New(&prometheusadapter.Options{Registerer: registry})
	require.NoError(t, err)
	_, err = prometheusadapter.New(&prometheusadapter.Options{Registerer: registry})
	require.ErrorContains(t, err, "register", "a second set on one registry is refused, not panicked")
}
