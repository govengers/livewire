package toolset_test

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/govengers/livewire/internal/toolfixture"
	"github.com/govengers/livewire/toolset"
)

// A toolset for an assistant that may read and create notes but not delete them, asked before
// every write; the call runs as the identity in ctx, through the API's own checks.
func ExampleNew() {
	fixture, err := toolfixture.New() // an engine and its catalogue (generate.NewCatalogue)
	if err != nil {
		fmt.Println(err)
		return
	}
	ctx := toolfixture.User(context.Background()) // the request's identity, as the authentication put it there
	// snippet:begin toolset
	tools, err := toolset.New(&toolset.Options{
		Engine: fixture.Engine, Catalogue: fixture.Catalogue,
		Agent:   "assistant", // the audit's real actor: agent:assistant
		Exclude: []string{"deleteNote"},
		Confirm: func(context.Context, *toolset.Call) error { return nil }, // ask the user; an error declines
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	answer, err := tools.Call(ctx, "createNote", json.RawMessage(`{"body":{"title":"buy milk"}}`), toolset.CallOptions{})
	// snippet:end toolset
	if err != nil {
		fmt.Println(err)
		return
	}
	var created toolfixture.Note
	_ = json.Unmarshal(answer, &created)
	fmt.Println(created.Title, fixture.Audit()[0].Identity.Real().Subject)
	_, err = tools.Call(ctx, "createNote", json.RawMessage(`{"body":{"title":"x","secret":"mine"}}`), toolset.CallOptions{})
	fmt.Println(err)
	// Output:
	// buy milk assistant
	// forbidden_field: a field of the request is not accessible secret: forbidden
}
