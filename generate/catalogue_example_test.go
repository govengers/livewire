package generate_test

import (
	"fmt"

	"github.com/govengers/livewire/generate"
	"github.com/govengers/livewire/internal/toolfixture"
)

// The catalogue of a registry: one tool per read, mutation and upload.
func ExampleNewCatalogue() {
	fixture, err := toolfixture.New()
	if err != nil {
		fmt.Println(err)
		return
	}
	registry := fixture.Engine.Registry()
	// snippet:begin catalogue
	catalogue, err := generate.NewCatalogue(registry, &generate.CatalogueOptions{Presence: false})
	if err != nil {
		fmt.Println(err)
		return
	}
	for _, tool := range catalogue.Tools() {
		fmt.Println(tool.Name, tool.Kind, tool.Mutating, tool.Method, tool.Path)
	}
	// snippet:end catalogue

	// Output:
	// getNotes read false GET /api/notes
	// createNote mutation true POST /api/notes
	// deleteNote mutation true DELETE /api/notes/{id}
	// attachNote upload true POST /api/notes/{id}/attachments
}
