package snoozebot

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeTestPrivateKey(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	b := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	p := filepath.Join(t.TempDir(), "app.pem")
	require.NoError(t, os.WriteFile(p, b, 0o600))
	return p
}
func appModeTestConfig(t *testing.T) Config {
	t.Helper()
	return Config{GitHubAppID: 123, GitHubAppIDRaw: "123", GitHubAppPrivateKeyFile: writeTestPrivateKey(t), WebhookSecret: "secret"}
}

func TestInstallationTransportCacheIsolationAndReuse(t *testing.T) {
	app := &App{Config: appModeTestConfig(t)}
	c1, err := app.getClient(10)
	require.NoError(t, err)
	c2, err := app.getClient(10)
	require.NoError(t, err)
	c3, err := app.getClient(20)
	require.NoError(t, err)
	assert.True(t, c1.Client().Transport == c2.Client().Transport)
	assert.True(t, c1.Client().Transport != c3.Client().Transport)
	assert.Len(t, app.appTransports, 2)
	app.evictInstallationTransport(10)
	c4, err := app.getClient(10)
	require.NoError(t, err)
	assert.True(t, c1.Client().Transport != c4.Client().Transport)
}

func TestInstallationTransportConcurrentFirstAccess(t *testing.T) {
	app := &App{Config: appModeTestConfig(t)}
	const workers = 24
	trs := make(chan http.RoundTripper, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			c, err := app.getClient(42)
			if err != nil {
				errs <- err
				return
			}
			trs <- c.Client().Transport
		}()
	}
	wg.Wait()
	close(trs)
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	var first http.RoundTripper
	for tr := range trs {
		if first == nil {
			first = tr
		} else {
			assert.True(t, first == tr)
		}
	}
	assert.Len(t, app.appTransports, 1)
}

func TestInstallationTransportReconstructedAfterRestart(t *testing.T) {
	cfg := appModeTestConfig(t)
	a := &App{Config: cfg}
	c1, err := a.getClient(77)
	require.NoError(t, err)
	b := &App{Config: cfg}
	c2, err := b.getClient(77)
	require.NoError(t, err)
	assert.True(t, c1.Client().Transport != c2.Client().Transport)
	assert.Len(t, a.appTransports, 1)
	assert.Len(t, b.appTransports, 1)
}

func TestInstallationTokenRenewal(t *testing.T) {
	app := &App{Config: appModeTestConfig(t)}
	_, err := app.getClient(99)
	require.NoError(t, err)
	tr := app.appTransports[99]
	require.NotNil(t, tr)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/app/installations/99/access_tokens", r.URL.Path)
		assert.NotEmpty(t, r.Header.Get("Authorization"))
		n := calls.Add(1)
		expires := time.Now().Add(time.Hour)
		token := "token-2"
		if n == 1 {
			expires = time.Now().Add(30 * time.Second)
			token = "token-1"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"token":%q,"expires_at":%q}`, token, expires.UTC().Format(time.RFC3339))
	}))
	defer srv.Close()
	tr.BaseURL = srv.URL
	t1, err := tr.Token(context.Background())
	require.NoError(t, err)
	t2, err := tr.Token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "token-1", t1)
	assert.Equal(t, "token-2", t2)
	assert.Equal(t, int32(2), calls.Load())
}
