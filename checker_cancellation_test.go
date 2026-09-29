package snoozebot

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-github/v62/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOwnershipLossCancelsInflightReminderRequest(t *testing.T) {
	requestStarted := make(chan struct{}, 1)
	requestCancelled := make(chan struct{}, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		requestStarted <- struct{}{}
		select {
		case <-r.Context().Done():
			requestCancelled <- struct{}{}
			return
		case <-time.After(2 * time.Second):
			w.WriteHeader(http.StatusCreated)
		}
	}))
	defer server.Close()

	baseURL, err := url.Parse(server.URL + "/")
	require.NoError(t, err)
	client := github.NewClient(server.Client())
	client.BaseURL = baseURL
	client.UploadURL = baseURL

	db, err := InitDB(filepath.Join(t.TempDir(), "cancel.db"))
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	require.NoError(t, InsertSnooze(db, "owner", "repo", 1, "alice", time.Now().Add(-time.Minute), 0))

	app := &App{
		Config: Config{GitHubToken: "pat"},
		Client: client,
		DB:     db,
	}
	claimed, err := app.claimSnooze(1, time.Now(), "worker-1")
	require.NoError(t, err)
	require.True(t, claimed)

	snooze := SnoozeRecord{ID: 1, RepoOwner: "owner", RepoName: "repo", IssueID: 1, Username: "alice"}
	result := make(chan error, 1)

	// Run the production posting method, but speed up the ownership check by
	// starting a separate production renewal loop with a short interval.
	ctx := context.Background()
	go func() {
		result <- app.postSnoozeReplyWithRenewInterval(ctx, snooze, "worker-1", 5*time.Millisecond)
	}()

	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("GitHub request did not start")
	}

	// Replace ownership while the HTTP request is blocked. The short renewal
	// interval above drives the same production ownership check used in normal
	// delivery; losing the claim must cancel the request without an external
	// context cancellation.
	_, err = db.Exec("UPDATE snoozes SET claim_owner='worker-2' WHERE id=1")
	require.NoError(t, err)

	select {
	case <-requestCancelled:
	case <-time.After(time.Second):
		t.Fatal("in-flight GitHub request did not observe cancellation")
	}

	select {
	case err := <-result:
		assert.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("PostSnoozeReply did not return after cancellation")
	}
}
