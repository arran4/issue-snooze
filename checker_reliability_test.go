package snoozebot

import (
	"context"
	"database/sql"
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

func TestClaimExpiryReclaimAndStaleOwnerIsolation(t *testing.T) {
	db, err := InitDB(filepath.Join(t.TempDir(), "claim.db"))
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	require.NoError(t, InsertSnooze(db, "owner", "repo", 1, "alice", time.Now().Add(-time.Minute), 1))
	app := &App{DB: db}

	now := time.Now()
	claimed, err := app.claimSnooze(1, now, "worker-1")
	require.NoError(t, err)
	require.True(t, claimed)

	claimed, err = app.claimSnooze(1, now.Add(6*time.Minute), "worker-2")
	require.NoError(t, err)
	require.True(t, claimed, "expired abandoned claim should be reclaimable")

	require.NoError(t, app.releaseSnooze(1, "worker-1"))
	var owner sql.NullString
	require.NoError(t, db.QueryRow("SELECT claim_owner FROM snoozes WHERE id=1").Scan(&owner))
	require.True(t, owner.Valid)
	assert.Equal(t, "worker-2", owner.String)

	assert.Error(t, app.deleteSnoozeIfOwned(1, "worker-1"))
	require.NoError(t, app.deleteSnoozeIfOwned(1, "worker-2"))
}

func TestFailedReminderPostReleasesClaimForRetry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"temporary failure"}`))
	}))
	defer server.Close()
	baseURL, err := url.Parse(server.URL + "/")
	require.NoError(t, err)
	client := github.NewClient(server.Client())
	client.BaseURL = baseURL
	client.UploadURL = baseURL

	db, err := InitDB(filepath.Join(t.TempDir(), "retry.db"))
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	require.NoError(t, InsertSnooze(db, "owner", "repo", 1, "alice", time.Now().Add(-time.Minute), 0))

	app := &App{
		Config: Config{GitHubToken: "pat"},
		Client: client,
		DB:     db,
	}
	app.CheckExpiredSnoozes(context.Background())

	var count int
	var lockedUntil, claimOwner sql.NullString
	require.NoError(t, db.QueryRow("SELECT COUNT(*), locked_until, claim_owner FROM snoozes WHERE id=1").Scan(&count, &lockedUntil, &claimOwner))
	assert.Equal(t, 1, count)
	assert.False(t, lockedUntil.Valid, "failed post should release lease")
	assert.False(t, claimOwner.Valid, "failed post should clear owner")
}

func TestPendingReminderSurvivesDatabaseReopen(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "reopen.db")
	db, err := InitDB(dbPath)
	require.NoError(t, err)
	require.NoError(t, InsertSnooze(db, "owner", "repo", 1, "alice", time.Now().Add(time.Hour), 99))
	require.NoError(t, db.Close())

	db, err = InitDB(dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	var count int
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM snoozes WHERE installation_id=99").Scan(&count))
	assert.Equal(t, 1, count)
}
