package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// TestARecoveredPanicPrintsNoCredentials pins LOG-009: a panic is answered with the error body
// and nothing is printed, so neither the session cookie nor the API key reaches a log; gin's
// default recovery printed both.
func TestARecoveredPanicPrintsNoCredentials(t *testing.T) {
	var printed bytes.Buffer
	gin.DefaultErrorWriter = &printed
	defer func() { gin.DefaultErrorWriter = gin.DefaultWriter }()
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(recovery())
	router.GET("/panic", func(*gin.Context) { panic("the handler failed") })
	router.GET("/abort", func(*gin.Context) { panic(http.ErrAbortHandler) })
	server := httptest.NewServer(router)
	defer server.Close()

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/panic", nil)
	require.NoError(t, err)
	request.Header.Set("Cookie", "__Host-auth_session=secret-session")
	request.Header.Set("X-API-Key", "todo_secret-key")
	response, err := server.Client().Do(request)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusInternalServerError, response.StatusCode)

	aborted, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/abort", nil)
	require.NoError(t, err)
	aborted.Header.Set("Cookie", "__Host-auth_session=secret-session")
	aborted.Header.Set("X-API-Key", "todo_secret-key")
	response, err = server.Client().Do(aborted)
	if err == nil {
		require.NoError(t, response.Body.Close())
	}
	require.NotContains(t, printed.String(), "secret-session")
	require.NotContains(t, printed.String(), "secret-key")
	require.Empty(t, printed.String())
	require.Error(t, err, "http.ErrAbortHandler reaches net/http, which drops the connection (D60)")
}
