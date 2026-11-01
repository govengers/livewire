package client_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/client"
)

type note struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// Example reads a list with the runtime directly; a generated client's GetNotes does the same
// with typed parameters.
func Example() {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		fmt.Println(request.Method, request.URL.Path, request.URL.RawQuery, request.Header.Get("Authorization"))
		_, _ = io.WriteString(writer, `{"limit":10,"offset":0,"total":1,"items":[{"id":"1","text":"hello"}]}`)
	}))
	defer server.Close()
	notes, err := client.New(&client.Options{BaseURL: server.URL + "/api/v1", APIKey: "secret"})
	if err != nil {
		panic(err)
	}
	parameters := client.Parameters{}
	parameters.Set("limit", 10)
	var page livewire.ListResponse[note]
	if err := notes.Read(context.Background(), "/notes", parameters, &page); err != nil {
		panic(err)
	}
	fmt.Println(page.Items[0].Text)
	// Output:
	// GET /api/v1/notes limit=10 Bearer secret
	// hello
}
