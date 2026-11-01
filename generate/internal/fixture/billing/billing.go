// Package billing holds DTOs whose Go names repeat the fixture package's: the generator
// qualifies both with their package's name.
package billing

// Part has the Go name of fixture.Part.
type Part struct {
	Invoice string `json:"invoice"`
}

// ErrorBody has the name of a library type.
type ErrorBody struct {
	Reason string `json:"reason"`
}

// Record has the name of a TypeScript global.
type Record struct {
	Body ErrorBody `json:"body"`
}
