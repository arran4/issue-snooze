package snoozebot

import (
	"os"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
)

func TestDBMigration(t *testing.T) {
	dbFile := "test_migration.db"
	defer func() {
		_ = os.Remove(dbFile)
	}()

	// Phase 1: Initialize with Old Schema
	db, err := InitDB(dbFile)
	assert.NoError(t, err)

	// Revert the migration to test upgrading (drop column trick for sqlite)
	_, err = db.Exec("ALTER TABLE snoozes DROP COLUMN installation_id")
	if err != nil {
		t.Logf("Drop column might fail if not supported on this SQLite version, but that's fine, we will recreate: %v", err)
		_, _ = db.Exec("DROP TABLE snoozes")
		query := `
		CREATE TABLE snoozes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			repo_owner TEXT NOT NULL,
			repo_name TEXT NOT NULL,
			issue_id INTEGER NOT NULL,
			username TEXT NOT NULL,
			target_time DATETIME NOT NULL
		)

		`
		_, err = db.Exec(query)
		assert.NoError(t, err)
	}

	// Insert data into old schema directly
	_, err = db.Exec("INSERT INTO snoozes (repo_owner, repo_name, issue_id, username, target_time) VALUES (?, ?, ?, ?, ?)", "owner1", "repo1", 1, "user1", time.Now().Format(time.RFC3339))
	assert.NoError(t, err)
	_ = db.Close()

	// Phase 2: Run InitDB again to apply migration
	db, err = InitDB(dbFile)
	assert.NoError(t, err)
	defer func() {
		_ = db.Close()
	}()

	// Insert a new record with installationID
	err = InsertSnooze(db, "owner2", "repo2", 2, "user2", time.Now().Add(time.Hour), 12345)
	assert.NoError(t, err)

	// Fetch all
	snoozes, err := GetExpiredSnoozes(db, time.Now().Add(time.Hour*2))
	assert.NoError(t, err)

	assert.Len(t, snoozes, 2)

	// Check migrated record
	var migrated SnoozeRecord
	var newRec SnoozeRecord
	for _, s := range snoozes {
		if s.IssueID == 1 {
			migrated = s
		} else {
			newRec = s
		}
	}

	assert.Equal(t, int64(0), migrated.InstallationID)
	assert.Equal(t, int64(12345), newRec.InstallationID)
}
