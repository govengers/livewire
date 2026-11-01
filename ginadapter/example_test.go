package ginadapter_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/gin-gonic/gin"
	"github.com/govengers/auth"
	"github.com/govengers/auth/authorization"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/ginadapter"
)

type greeting struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

func Example() {
	registry, err := livewire.NewRegistry(&livewire.RegistryOptions{BasePath: "/api"})
	if err != nil {
		fmt.Println(err)
		return
	}
	err = registry.Add(livewire.NewList("greetings", "/greetings", &livewire.ListOptions[greeting, livewire.NoParameters]{
		Read: authorization.IsUser(),
		Load: func(context.Context, *livewire.NoParameters) ([]greeting, error) {
			return []greeting{{ID: "1", Text: "hello"}}, nil
		},
	}))
	if err != nil {
		fmt.Println(err)
		return
	}
	people := &identities{people: map[string]*auth.Identity{"ada": {Provider: "example", Subject: "ada", Active: true}}}
	engine, err := livewire.NewEngine(&livewire.EngineOptions{Registry: registry, IdentitySource: people, Transactor: &transactor{}, SameHostOnly: true})
	if err != nil {
		fmt.Println(err)
		return
	}
	defer func() { _ = engine.Close(context.Background()) }() // nothing is open; Close only ends the engine

	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(func(ginContext *gin.Context) { // stands in for the application's authentication
		identity := people.get(ginContext.GetHeader(subjectHeader))
		ginContext.Request = ginContext.Request.WithContext(auth.WithIdentity(ginContext.Request.Context(), identity))
	})
	if err := ginadapter.RegisterRoutes(router, &ginadapter.Options{Engine: engine}); err != nil {
		fmt.Println(err)
		return
	}
	router.GET("/api/socket", ginadapter.SocketHandler(engine))

	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/greetings", nil)
	request.Header.Set(subjectHeader, "ada")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	fmt.Println(recorder.Code, recorder.Body.String())
	// Output: 200 {"limit":1,"offset":0,"total":1,"items":[{"id":"1","text":"hello"}]}
}
