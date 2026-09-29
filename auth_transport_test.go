package snoozebot

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInstallationTokenRenewalAndReuse(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/app/installations/10/access_tokens", r.URL.Path)
		assert.Contains(t, r.Header.Get("Authorization"), "Bearer ")

		n := requests.Add(1)
		expiresAt := time.Now().Add(time.Hour)
		token := "token-2"
		if n == 1 {
			// ghinstallation refreshes one minute before expiry, so this token
			// must be refreshed on the next Token call.
			expiresAt = time.Now().Add(30 * time.Second)
			token = "token-1"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"token":%q,"expires_at":%q}`, token, expiresAt.UTC().Format(time.RFC3339))
	}))
	defer server.Close()

	app := &App{Config: Config{
		GitHubAppID:             123,
		GitHubAppPrivateKeyFile: writeTestPrivateKey(t),
		WebhookSecret:           "secret",
	}}
	_, err := app.getClient(10)
	require.NoError(t, err)

	app.appTransportMux.Lock()
	transport := app.appTransports[10]
	app.appTransportMux.Unlock()
	require.NotNil(t, transport)
	transport.BaseURL = server.URL

	token, err := transport.Token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "token-1", token)

	token, err = transport.Token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "token-2", token)

	token, err = transport.Token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "token-2", token)
	assert.Equal(t, int32(2), requests.Load(), "valid renewed token should be reused without another refresh")
}
