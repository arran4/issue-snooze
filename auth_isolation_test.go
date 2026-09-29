package snoozebot

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInstallationTokenFailureIsIsolated(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/installations/10/"):
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = fmt.Fprint(w, `{"message":"installation 10 failure"}`)
		case strings.Contains(r.URL.Path, "/installations/20/"):
			_, _ = fmt.Fprintf(w, `{"token":"install-20","expires_at":%q}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	app := &App{Config: Config{
		GitHubAppID:             123,
		GitHubAppPrivateKeyFile: writeTestPrivateKey(t),
		WebhookSecret:           "secret",
	}}
	_, err := app.getClient(10)
	require.NoError(t, err)
	_, err = app.getClient(20)
	require.NoError(t, err)

	app.appTransportMux.Lock()
	transport10 := app.appTransports[10]
	transport20 := app.appTransports[20]
	app.appTransportMux.Unlock()
	require.NotNil(t, transport10)
	require.NotNil(t, transport20)
	transport10.BaseURL = server.URL
	transport20.BaseURL = server.URL

	_, err = transport10.Token(context.Background())
	assert.Error(t, err)

	token, err := transport20.Token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "install-20", token)

	_, err = transport10.Token(context.Background())
	assert.Error(t, err, "failed installation should remain independently retryable")
	token, err = transport20.Token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "install-20", token)
}
