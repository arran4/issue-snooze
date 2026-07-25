package main

import (
	"database/sql"
	"fmt"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// SnoozeRecord represents a snooze entry in the database
type SnoozeRecord struct {
	ID         int
	RepoOwner  string
	RepoName   string
	IssueID    int
	Username   string
	TargetTime time.Time
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
		return nil, fmt.Errorf("failed to create tables: %w", err)
	}

	return db, nil
}

// InsertSnooze adds a new snooze record to the database
func InsertSnooze(db *sql.DB, owner, repo string, issueID int, username string, targetTime time.Time) error {
	query := `
	INSERT INTO snoozes (repo_owner, repo_name, issue_id, username, target_time)
	VALUES (?, ?, ?, ?, ?)
	`
	_, err := db.Exec(query, owner, repo, issueID, username, targetTime.UTC().Format(time.RFC3339))
	return err
}

// GetExpiredSnoozes retrieves all snooze records where the target time has passed
func GetExpiredSnoozes(db *sql.DB, now time.Time) ([]SnoozeRecord, error) {
	query := `
	SELECT id, repo_owner, repo_name, issue_id, username, target_time
	FROM snoozes
	WHERE target_time <= ?
	`
	rows, err := db.Query(query, now.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var expired []SnoozeRecord
	for rows.Next() {
		var r SnoozeRecord
		var targetTimeStr string
		err := rows.Scan(&r.ID, &r.RepoOwner, &r.RepoName, &r.IssueID, &r.Username, &targetTimeStr)
		if err != nil {
			return nil, err
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
