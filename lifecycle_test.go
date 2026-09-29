package snoozebot

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-github/v62/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInstallationLifecyclePreservesOrRemovesScopedReminders(t *testing.T) {
	db, err := InitDB(filepath.Join(t.TempDir(), "lifecycle.db"))
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	config := Config{
		GitHubAppID:             123,
		GitHubAppPrivateKeyFile: writeTestPrivateKey(t),
		WebhookSecret:           "secret",
	}
	app := &App{Config: config, DB: db}

	require.NoError(t, InsertSnooze(db, "owner", "repo", 1, "alice", time.Now().Add(time.Hour), 10))
	require.NoError(t, InsertSnooze(db, "owner", "repo", 2, "alice", time.Now().Add(time.Hour), 20))
	require.NoError(t, InsertSnooze(db, "owner", "other", 3, "alice", time.Now().Add(time.Hour), 10))

	_, err = app.getClient(10)
	require.NoError(t, err)
	app.appTransportMux.Lock()
	_, cached := app.appTransports[10]
	app.appTransportMux.Unlock()
	require.True(t, cached)

	suspend := "suspend"
	require.NoError(t, app.handleInstallationEvent(&github.InstallationEvent{
		Action:       &suspend,
		Installation: &github.Installation{ID: github.Int64(10)},
	}))
	app.appTransportMux.Lock()
	_, cached = app.appTransports[10]
	app.appTransportMux.Unlock()
	assert.False(t, cached)

	var count int
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM snoozes WHERE installation_id=10").Scan(&count))
	assert.Equal(t, 2, count, "suspension must preserve pending reminders")

	unsuspend := "unsuspend"
	require.NoError(t, app.handleInstallationEvent(&github.InstallationEvent{
		Action:       &unsuspend,
		Installation: &github.Installation{ID: github.Int64(10)},
	}))
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM snoozes WHERE installation_id=10").Scan(&count))
	assert.Equal(t, 2, count)

	removed := "removed"
	require.NoError(t, app.handleInstallationRepositoriesEvent(&github.InstallationRepositoriesEvent{
		Action:       &removed,
		Installation: &github.Installation{ID: github.Int64(10)},
		RepositoriesRemoved: []*github.Repository{{
			Name:     github.String("repo"),
			FullName: github.String("owner/repo"),
			Owner:    &github.User{Login: github.String("owner")},
		}},
	}))
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM snoozes WHERE repo_owner='owner' AND repo_name='repo' AND installation_id=10").Scan(&count))
	assert.Zero(t, count)
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM snoozes WHERE repo_owner='owner' AND repo_name='repo' AND installation_id=20").Scan(&count))
	assert.Equal(t, 1, count, "repository removal must not cross installation boundaries")

	deleted := "deleted"
	require.NoError(t, app.handleInstallationEvent(&github.InstallationEvent{
		Action:       &deleted,
		Installation: &github.Installation{ID: github.Int64(10)},
	}))
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM snoozes WHERE installation_id=10").Scan(&count))
	assert.Zero(t, count)
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM snoozes WHERE installation_id=20").Scan(&count))
	assert.Equal(t, 1, count)
}

func TestInstallationLifecycleDBFailureIsReturned(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	require.NoError(t, db.Close())
	app := &App{DB: db}
	deleted := "deleted"
	err = app.handleInstallationEvent(&github.InstallationEvent{
		Action:       &deleted,
		Installation: &github.Installation{ID: github.Int64(10)},
	})
	assert.Error(t, err)
}
