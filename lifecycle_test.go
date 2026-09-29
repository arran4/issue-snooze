package snoozebot

import (
	"testing"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v62/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRepositoryLifecycleReconciliation(t *testing.T) {
	db, err := InitDB(t.TempDir() + "/lifecycle.db")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	app := &App{DB: db}
	future := time.Now().Add(time.Hour)
	require.NoError(t, InsertSnooze(db, "oldowner", "oldrepo", 1, "alice", future, 10))
	require.NoError(t, InsertSnooze(db, "oldowner", "oldrepo", 2, "bob", future, 20))
	rename, oldName := "renamed", "oldrepo"
	require.NoError(t, app.handleRepositoryEvent(&github.RepositoryEvent{Action: &rename, Installation: &github.Installation{ID: github.Int64(10)}, Repo: &github.Repository{Name: github.String("newrepo"), Owner: &github.User{Login: github.String("oldowner")}}, Changes: &github.EditChange{Repo: &github.EditRepo{Name: &github.RepoName{From: &oldName}}}}))
	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM snoozes WHERE repo_owner='oldowner' AND repo_name='newrepo' AND installation_id=10`).Scan(&count))
	assert.Equal(t, 1, count)
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM snoozes WHERE repo_owner='oldowner' AND repo_name='oldrepo' AND installation_id=20`).Scan(&count))
	assert.Equal(t, 1, count)
	transfer, oldOwner := "transferred", "oldowner"
	require.NoError(t, app.handleRepositoryEvent(&github.RepositoryEvent{Action: &transfer, Installation: &github.Installation{ID: github.Int64(10)}, Repo: &github.Repository{Name: github.String("newrepo"), Owner: &github.User{Login: github.String("newowner")}}, Changes: &github.EditChange{Owner: &github.EditOwner{OwnerInfo: &github.OwnerInfo{User: &github.User{Login: &oldOwner}}}}}))
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM snoozes WHERE repo_owner='newowner' AND repo_name='newrepo' AND installation_id=10`).Scan(&count))
	assert.Equal(t, 1, count)
	require.NoError(t, InsertSnooze(db, "before", "old-name", 3, "carol", future, 30))
	beforeOwner, beforeName := "before", "old-name"
	require.NoError(t, app.handleRepositoryEvent(&github.RepositoryEvent{Action: &transfer, Installation: &github.Installation{ID: github.Int64(30)}, Repo: &github.Repository{Name: github.String("new-name"), Owner: &github.User{Login: github.String("after")}}, Changes: &github.EditChange{Owner: &github.EditOwner{OwnerInfo: &github.OwnerInfo{Org: &github.User{Login: &beforeOwner}}}, Repo: &github.EditRepo{Name: &github.RepoName{From: &beforeName}}}}))
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM snoozes WHERE repo_owner='after' AND repo_name='new-name' AND installation_id=30`).Scan(&count))
	assert.Equal(t, 1, count)
}

func TestRepositoryLifecycleMissingOldIdentityFails(t *testing.T) {
	db, err := InitDB(t.TempDir() + "/missing.db")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	app := &App{DB: db}
	require.NoError(t, InsertSnooze(db, "old", "repo", 1, "a", time.Now().Add(time.Hour), 10))
	action := "transferred"
	err = app.handleRepositoryEvent(&github.RepositoryEvent{Action: &action, Installation: &github.Installation{ID: github.Int64(10)}, Repo: &github.Repository{Name: github.String("repo"), Owner: &github.User{Login: github.String("new")}}, Changes: &github.EditChange{}})
	assert.Error(t, err)
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM snoozes WHERE repo_owner='old' AND repo_name='repo' AND installation_id=10`).Scan(&n))
	assert.Equal(t, 1, n)
}

func TestInstallationTargetRenameIsInstallationScoped(t *testing.T) {
	db, err := InitDB(t.TempDir() + "/target.db")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	app := &App{DB: db}
	future := time.Now().Add(time.Hour)
	require.NoError(t, InsertSnooze(db, "oldorg", "a", 1, "a", future, 10))
	require.NoError(t, InsertSnooze(db, "oldorg", "b", 2, "b", future, 20))
	action, old := "renamed", "oldorg"
	require.NoError(t, app.handleInstallationTargetEvent(&github.InstallationTargetEvent{Action: &action, Installation: &github.Installation{ID: github.Int64(10)}, Account: &github.User{Login: github.String("neworg")}, Changes: &github.InstallationChanges{Login: &github.InstallationLoginChange{From: &old}}}))
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM snoozes WHERE repo_owner='neworg' AND installation_id=10`).Scan(&n))
	assert.Equal(t, 1, n)
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM snoozes WHERE repo_owner='oldorg' AND installation_id=20`).Scan(&n))
	assert.Equal(t, 1, n)
}

func TestInstallationLifecyclePolicies(t *testing.T) {
	for _, action := range []string{"suspend", "unsuspend", "new_permissions_accepted"} {
		t.Run(action, func(t *testing.T) {
			app := &App{appTransports: map[int64]*ghinstallation.Transport{10: {}}}
			require.NoError(t, app.handleInstallationEvent(&github.InstallationEvent{Action: github.String(action), Installation: &github.Installation{ID: github.Int64(10)}}))
			_, ok := app.appTransports[10]
			assert.False(t, ok)
		})
	}
	db, err := InitDB(t.TempDir() + "/install.db")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	future := time.Now().Add(time.Hour)
	require.NoError(t, InsertSnooze(db, "owner", "repo", 1, "a", future, 10))
	require.NoError(t, InsertSnooze(db, "owner", "repo", 2, "b", future, 20))
	app := &App{DB: db, appTransports: map[int64]*ghinstallation.Transport{10: {}}}
	deleted := "deleted"
	require.NoError(t, app.handleInstallationEvent(&github.InstallationEvent{Action: &deleted, Installation: &github.Installation{ID: github.Int64(10)}}))
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM snoozes WHERE installation_id=10`).Scan(&n))
	assert.Equal(t, 0, n)
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM snoozes WHERE installation_id=20`).Scan(&n))
	assert.Equal(t, 1, n)
}

func TestRepositoryRemovalIsInstallationScoped(t *testing.T) {
	db, err := InitDB(t.TempDir() + "/remove.db")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	future := time.Now().Add(time.Hour)
	require.NoError(t, InsertSnooze(db, "owner", "repo", 1, "a", future, 10))
	require.NoError(t, InsertSnooze(db, "owner", "repo", 2, "b", future, 20))
	app := &App{DB: db}
	action := "removed"
	require.NoError(t, app.handleInstallationRepositoriesEvent(&github.InstallationRepositoriesEvent{Action: &action, Installation: &github.Installation{ID: github.Int64(10)}, RepositoriesRemoved: []*github.Repository{{FullName: github.String("owner/repo")}}}))
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM snoozes WHERE installation_id=10`).Scan(&n))
	assert.Equal(t, 0, n)
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM snoozes WHERE installation_id=20`).Scan(&n))
	assert.Equal(t, 1, n)
}
