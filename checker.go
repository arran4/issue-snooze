package snoozebot

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/google/go-github/v62/github"
)

// StartBackgroundChecker starts a goroutine that periodically checks for expired snoozes
func (a *App) StartBackgroundChecker(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	go func() {
		for {
			select {
			case <-ctx.Done():
				ticker.Stop()
				return
			case <-ticker.C:
				a.CheckExpiredSnoozes(ctx)
			}
		}
	}()
}

// CheckExpiredSnoozes queries the database for expired snoozes and posts comments
func (a *App) CheckExpiredSnoozes(ctx context.Context) {
	now := time.Now()
	expired, err := GetExpiredSnoozes(a.DB, now)
	if err != nil {
		log.Printf("Error getting expired snoozes: %v", err)
		return
	}

	for _, snooze := range expired {
		// Update 'now' per iteration to ensure locks don't expire prematurely on long batches
		currentNow := time.Now()

		// 4. Reminder reliability/concurrency: implement a claim model.
		// We try to claim the snooze by setting a locked_until time.
		// If another worker has already claimed it, this will return false.
		claimed, err := a.claimSnooze(snooze.ID, currentNow)
		if err != nil {
			log.Printf("Failed to attempt claiming snooze ID %d: %v", snooze.ID, err)
			continue
		}
		if !claimed {
			// Another worker claimed it
			continue
		}

		log.Printf("Processing expired snooze for @%s on %s/%s#%d",
			snooze.Username, snooze.RepoOwner, snooze.RepoName, snooze.IssueID)

		err = a.PostSnoozeReply(ctx, snooze)
		if err != nil {
			log.Printf("Failed to post snooze reply for ID %d: %v. Releasing claim for retry.", snooze.ID, err)
			// Release the claim so it can be retried later
			_ = a.releaseSnooze(snooze.ID)
			continue
		}

		// Successful post removes/completes it
		err = DeleteSnooze(a.DB, snooze.ID)
		if err != nil {
			log.Printf("Failed to delete processed snooze ID %d: %v", snooze.ID, err)
		}
	}
}

func (a *App) claimSnooze(id int, now time.Time) (bool, error) {
	// Lock for 5 minutes
	lockedUntil := now.Add(5 * time.Minute).UTC().Format(time.RFC3339)
	res, err := a.DB.Exec(`UPDATE snoozes SET locked_until = ? WHERE id = ? AND (locked_until IS NULL OR locked_until < ?)`, lockedUntil, id, now.UTC().Format(time.RFC3339))
	if err != nil {
		return false, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}

func (a *App) releaseSnooze(id int) error {
	_, err := a.DB.Exec(`UPDATE snoozes SET locked_until = NULL WHERE id = ?`, id)
	return err
}

// PostSnoozeReply posts a comment to the GitHub issue tagging the user
func (a *App) PostSnoozeReply(ctx context.Context, snooze SnoozeRecord) error {
	client, err := a.getClient(snooze.InstallationID)
	if err != nil {
		return fmt.Errorf("could not get GitHub client for reply: %w", err)
	}
	if client == nil {
		return fmt.Errorf("GitHub client is not initialized")
	}

	body := fmt.Sprintf("Hey @%s, your snooze is over!", snooze.Username)
	comment := &github.IssueComment{
		Body: &body,
	}

	_, _, err = client.Issues.CreateComment(ctx, snooze.RepoOwner, snooze.RepoName, snooze.IssueID, comment)
	return err
}
