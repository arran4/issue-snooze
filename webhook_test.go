package snoozebot

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-github/v62/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testGitHubClient(t *testing.T, userStatus int) (*github.Client, func()) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/users/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if userStatus != http.StatusOK {
			w.WriteHeader(userStatus)
			_, _ = fmt.Fprint(w, `{"message":"test error"}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"login":"alice","location":"UTC"}`)
	}))
	baseURL, err := url.Parse(server.URL + "/")
	require.NoError(t, err)
	client := github.NewClient(server.Client())
	client.BaseURL = baseURL
	client.UploadURL = baseURL
	return client, server.Close
}

func signedIssueCommentRequest(t *testing.T, secret, delivery, action, body, userType string) *http.Request {
	t.Helper()
	if action == "" {
		action = "created"
	}
	if userType == "" {
		userType = "User"
	}
	payload := []byte(fmt.Sprintf(`{
		"action":%q,
		"issue":{"number":7},
		"comment":{"body":%q,"user":{"login":"alice","type":%q}},
		"repository":{"name":"repo","full_name":"owner/repo","owner":{"login":"owner"}},
		"sender":{"login":"alice","type":%q}
	}`, action, body, userType, userType))
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(payload))
	req.Header.Set("X-GitHub-Event", "issue_comment")
	req.Header.Set("X-Hub-Signature-256", webhookSignature(secret, payload))
	req.Header.Set("Content-Type", "application/json")
	if delivery != "" {
		req.Header.Set("X-GitHub-Delivery", delivery)
	}
	return req
}

func TestIssueCommentWebhookClientErrorsAndFilters(t *testing.T) {
	const secret = "secret"
	client, closeServer := testGitHubClient(t, http.StatusOK)
	defer closeServer()

	db, err := InitDB(filepath.Join(t.TempDir(), "webhook.db"))
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	app := &App{
		Config: Config{GitHubToken: "pat", WebhookSecret: secret, BotCommand: "@snooze"},
		Client: client,
		DB:     db,
	}

	rr := httptest.NewRecorder()
	app.handleWebhook(rr, signedIssueCommentRequest(t, secret, "", "created", "@snooze tomorrow", "User"))
	assert.Equal(t, http.StatusBadRequest, rr.Code)

	rr = httptest.NewRecorder()
	app.handleWebhook(rr, signedIssueCommentRequest(t, secret, "bad-time", "created", "@snooze definitely-not-a-date", "User"))
	assert.Equal(t, http.StatusBadRequest, rr.Code)

	rr = httptest.NewRecorder()
	app.handleWebhook(rr, signedIssueCommentRequest(t, secret, "", "edited", "@snooze tomorrow", "User"))
	assert.Equal(t, http.StatusOK, rr.Code)

	rr = httptest.NewRecorder()
	app.handleWebhook(rr, signedIssueCommentRequest(t, secret, "", "created", "@snooze tomorrow", "Bot"))
	assert.Equal(t, http.StatusOK, rr.Code)

	var count int
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM snoozes").Scan(&count))
	assert.Zero(t, count)
}

func TestIssueCommentWebhookDuplicateSurvivesReopen(t *testing.T) {
	const secret = "secret"
	client, closeServer := testGitHubClient(t, http.StatusOK)
	defer closeServer()

	dbPath := filepath.Join(t.TempDir(), "duplicate.db")
	db, err := InitDB(dbPath)
	require.NoError(t, err)
	app := &App{
		Config: Config{GitHubToken: "pat", WebhookSecret: secret, BotCommand: "@snooze"},
		Client: client,
		DB:     db,
	}

	rr := httptest.NewRecorder()
	app.handleWebhook(rr, signedIssueCommentRequest(t, secret, "delivery-1", "created", "@snooze tomorrow", "User"))
	require.Equal(t, http.StatusOK, rr.Code)

	rr = httptest.NewRecorder()
	app.handleWebhook(rr, signedIssueCommentRequest(t, secret, "delivery-1", "created", "@snooze tomorrow", "User"))
	require.Equal(t, http.StatusOK, rr.Code)

	var count int
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM snoozes").Scan(&count))
	assert.Equal(t, 1, count)
	require.NoError(t, db.Close())

	db, err = InitDB(dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	app.DB = db

	rr = httptest.NewRecorder()
	app.handleWebhook(rr, signedIssueCommentRequest(t, secret, "delivery-1", "created", "@snooze tomorrow", "User"))
	require.Equal(t, http.StatusOK, rr.Code)
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM snoozes").Scan(&count))
	assert.Equal(t, 1, count)
}

func TestIssueCommentWebhookAppModeMissingInstallation(t *testing.T) {
	const secret = "secret"
	db, err := InitDB(filepath.Join(t.TempDir(), "app-mode.db"))
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	app := &App{
		Config: Config{
			GitHubAppID:             123,
			GitHubAppPrivateKeyFile: writeTestPrivateKey(t),
			WebhookSecret:           secret,
			BotCommand:              "@snooze",
		},
		DB: db,
	}
	rr := httptest.NewRecorder()
	app.handleWebhook(rr, signedIssueCommentRequest(t, secret, "delivery-app", "created", "@snooze tomorrow", "User"))
	assert.Equal(t, http.StatusInternalServerError, rr.Code)

	var count int
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM snoozes").Scan(&count))
	assert.Zero(t, count)
}

func TestIssueCommentWebhookStorageFailureReturns500(t *testing.T) {
	const secret = "secret"
	client, closeServer := testGitHubClient(t, http.StatusOK)
	defer closeServer()

	db, err := InitDB(filepath.Join(t.TempDir(), "closed.db"))
	require.NoError(t, err)
	require.NoError(t, db.Close())
	app := &App{
		Config: Config{GitHubToken: "pat", WebhookSecret: secret, BotCommand: "@snooze"},
		Client: client,
		DB:     db,
	}

	rr := httptest.NewRecorder()
	app.handleWebhook(rr, signedIssueCommentRequest(t, secret, "delivery-storage", "created", "@snooze tomorrow", "User"))
	assert.Equal(t, http.StatusInternalServerError, rr.Code)
}
