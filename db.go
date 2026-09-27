package snoozebot

import (
	"database/sql"
	"fmt"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// SnoozeRecord represents a snooze entry in the database
type SnoozeRecord struct {
	ID             int
	RepoOwner      string
	RepoName       string
	IssueID        int
	Username       string
	TargetTime     time.Time
	InstallationID int64
}

// InitDB initializes the SQLite database and creates the necessary tables
func InitDB(dbPath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	query := `
	CREATE TABLE IF NOT EXISTS snoozes (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		repo_owner TEXT NOT NULL,
		repo_name TEXT NOT NULL,
		issue_id INTEGER NOT NULL,
		username TEXT NOT NULL,
		target_time DATETIME NOT NULL
	);
	`
	_, err = db.Exec(query)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to create tables: %w", err)
	}

	// Make the DB migration fail safely if it's a real failure
	var exists bool
	err = db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('snoozes') WHERE name='installation_id'").Scan(&exists)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to inspect schema for migration: %w", err)
	}
	if !exists {
		_, err = db.Exec(`ALTER TABLE snoozes ADD COLUMN installation_id INTEGER DEFAULT 0`)
		if err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("failed to apply migration for installation_id: %w", err)
		}
	}

	queryDeliveries := `
	CREATE TABLE IF NOT EXISTS processed_deliveries (
		delivery_id TEXT PRIMARY KEY,
		processed_at DATETIME NOT NULL
	);
	`
	_, err = db.Exec(queryDeliveries)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to create processed_deliveries table: %w", err)
	}

	return db, nil
}

// InsertSnooze adds a new snooze record to the database
func InsertSnooze(db *sql.DB, owner, repo string, issueID int, username string, targetTime time.Time, installationID int64) error {
	query := `
	INSERT INTO snoozes (repo_owner, repo_name, issue_id, username, target_time, installation_id)
	VALUES (?, ?, ?, ?, ?, ?)
	`
	_, err := db.Exec(query, owner, repo, issueID, username, targetTime.UTC().Format(time.RFC3339), installationID)
	return err
}

// GetExpiredSnoozes retrieves all snooze records where the target time has passed
func GetExpiredSnoozes(db *sql.DB, now time.Time) ([]SnoozeRecord, error) {
	query := `
	SELECT id, repo_owner, repo_name, issue_id, username, target_time, installation_id
	FROM snoozes
	WHERE target_time <= ?
	`
	rows, err := db.Query(query, now.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = rows.Close()
	}()

	var expired []SnoozeRecord
	for rows.Next() {
		var r SnoozeRecord
		var targetTimeStr string
		var installationID sql.NullInt64
		err := rows.Scan(&r.ID, &r.RepoOwner, &r.RepoName, &r.IssueID, &r.Username, &targetTimeStr, &installationID)
		if err != nil {
			return nil, err
		}

		if installationID.Valid {
			r.InstallationID = installationID.Int64
		}

		t, err := time.Parse(time.RFC3339, targetTimeStr)
		if err == nil {
			r.TargetTime = t
		}
		expired = append(expired, r)
	}
	return expired, rows.Err()
}

// DeleteSnooze removes a snooze record from the database by ID
func DeleteSnooze(db *sql.DB, id int) error {
	query := `DELETE FROM snoozes WHERE id = ?`
	_, err := db.Exec(query, id)
	return err
}

// DeleteSnoozesByInstallation removes all snoozes associated with a specific installation ID
func DeleteSnoozesByInstallation(db *sql.DB, installationID int64) error {
	query := `DELETE FROM snoozes WHERE installation_id = ?`
	_, err := db.Exec(query, installationID)
	return err
}

// DeleteSnoozesByRepo removes all snoozes associated with a specific repository
func DeleteSnoozesByRepo(db *sql.DB, repoFullName string) error {
	// The DB stores repo_owner and repo_name separately.
	// Since repoFullName is typically "owner/name", we need to delete by matching those.
	// It's safer to just do a LIKE query or split it, but splitting is safer if we ensure it has a slash.
	query := `DELETE FROM snoozes WHERE repo_owner || '/' || repo_name = ?`
	_, err := db.Exec(query, repoFullName)
	return err
}
