package prometheusadapter_test

import (
	"context"
	"fmt"
	"net/http"

	"github.com/govengers/auth"
	"github.com/govengers/auth/authorization"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/prometheusadapter"
)

type greeting struct {
	ID string `json:"id"`
}

// noTransactor begins transactions that do nothing; a real application passes
// postgresadapter's Transactor.
type noTransactor struct{}

func (noTransactor) Begin(context.Context, livewire.TransactionDetails) (livewire.Transaction, error) {
	return noTransaction{}, nil
}

type noTransaction struct{}

func (noTransaction) Commit(context.Context) error   { return nil }
func (noTransaction) Rollback(context.Context) error { return nil }
func (noTransaction) Handle() any                    { return nil }

// anyone is the IdentitySource of the example; a real application passes session.Guard.
type anyone struct{}

func (anyone) Identify(context.Context, *http.Request) (*auth.Identity, error) {
	return &auth.Identity{Provider: "example", Subject: "ada", Active: true}, nil
}

func Example() {
	metricsRegistry := prometheus.NewRegistry() // the application's own, built in main (MET-002)
	metrics, err := prometheusadapter.New(&prometheusadapter.Options{Registerer: metricsRegistry, Namespace: "shop"})
	if err != nil {
		fmt.Println(err)
		return
	}
	registry, err := livewire.NewRegistry(&livewire.RegistryOptions{Observer: metrics.Observe}) // the one Observer (D49)
	if err != nil {
		fmt.Println(err)
		return
	}
	err = registry.Add(livewire.NewList("greetings", "/greetings", &livewire.ListOptions[greeting, livewire.NoParameters]{
		Read: authorization.IsUser(),
		Load: func(context.Context, *livewire.NoParameters) ([]greeting, error) { return []greeting{{ID: "1"}}, nil },
	}))
	if err != nil {
		fmt.Println(err)
		return
	}
	engine, err := livewire.NewEngine(&livewire.EngineOptions{Registry: registry, IdentitySource: anyone{}, Transactor: noTransactor{}, SameHostOnly: true})
	if err != nil {
		fmt.Println(err)
		return
	}
	ctx := auth.WithIdentity(context.Background(), &auth.Identity{Provider: "example", Subject: "ada", Active: true})
	_, _ = engine.Read(ctx, livewire.ReadRequest{Target: "greetings"})
	_, _ = engine.Read(context.Background(), livewire.ReadRequest{Target: "greetings"})

	fmt.Println(testutil.CollectAndCount(metricsRegistry, "shop_livewire_units_total"))
	// Output: 2
}
