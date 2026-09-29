package snoozebot

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type testRoundTripper func(*http.Request) (*http.Response, error)

func (f testRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestReminderPermissionFailureEvictsAuthAndFreshRetryRecovers(t *testing.T) {
	originalDefaultTransport := http.DefaultTransport
	defer func() { http.DefaultTransport = originalDefaultTransport }()

	var commentStatus atomic.Int32
	commentStatus.Store(http.StatusForbidden)
	var tokenRequests atomic.Int32
	var commentRequests atomic.Int32

	http.DefaultTransport = testRoundTripper(func(req *http.Request) (*http.Response, error) {
		status := http.StatusOK
		var body string
		switch {
		case strings.HasPrefix(req.URL.Path, "/app/installations/10/access_tokens"):
			tokenRequests.Add(1)
			body = fmt.Sprintf(`{"token":"token-%d","expires_at":%q}`, tokenRequests.Load(), time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		case strings.HasSuffix(req.URL.Path, "/repos/owner/repo/issues/1/comments"):
			commentRequests.Add(1)
			status = int(commentStatus.Load())
			if status == http.StatusCreated {
				body = `{"id":123,"body":"ok"}`
			} else {
				body = `{"message":"forbidden"}`
			}
		default:
			status = http.StatusNotFound
			body = `{"message":"unexpected test URL"}`
		}
		return &http.Response{
			StatusCode: status,
			Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})

	db, err := InitDB(t.TempDir() + "/permissions.db")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	require.NoError(t, InsertSnooze(db, "owner", "repo", 1, "alice", time.Now().Add(-time.Minute), 10))

	app := &App{
		Config: Config{
			GitHubAppID:             123,
			GitHubAppPrivateKeyFile: writeTestPrivateKey(t),
			WebhookSecret:           "secret",
		},
		DB: db,
	}
	snooze := SnoozeRecord{
		ID:             1,
		RepoOwner:      "owner",
		RepoName:       "repo",
		IssueID:        1,
		Username:       "alice",
		InstallationID: 10,
	}

	err = app.PostSnoozeReply(context.Background(), snooze, "worker-1")
	require.Error(t, err)
	app.appTransportMux.Lock()
	_, cached := app.appTransports[10]
	app.appTransportMux.Unlock()
	assert.False(t, cached, "401/403 must evict cached installation auth")

	commentStatus.Store(http.StatusCreated)
	err = app.PostSnoozeReply(context.Background(), snooze, "worker-1")
	require.NoError(t, err)
	app.appTransportMux.Lock()
	_, cached = app.appTransports[10]
	app.appTransportMux.Unlock()
	assert.True(t, cached, "retry should build fresh installation auth")
	assert.True(t, tokenRequests.Load() >= 2, "retry should obtain a fresh installation token")
	assert.Equal(t, int32(2), commentRequests.Load())
}
