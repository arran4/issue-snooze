package main

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
		log.Printf("Processing expired snooze for @%s on %s/%s#%d",
			snooze.Username, snooze.RepoOwner, snooze.RepoName, snooze.IssueID)

		err := a.PostSnoozeReply(ctx, snooze)
		if err != nil {
			log.Printf("Failed to post snooze reply for ID %d: %v", snooze.ID, err)
			continue
		}

		err = DeleteSnooze(a.DB, snooze.ID)
		if err != nil {
			log.Printf("Failed to delete processed snooze ID %d: %v", snooze.ID, err)
		}
	}
}

// PostSnoozeReply posts a comment to the GitHub issue tagging the user
func (a *App) PostSnoozeReply(ctx context.Context, snooze SnoozeRecord) error {
	if a.Client == nil {
		return fmt.Errorf("GitHub client is not initialized")
	}

	body := fmt.Sprintf("Hey @%s, your snooze is over!", snooze.Username)
	comment := &github.IssueComment{
		Body: &body,
	}

	_, _, err := a.Client.Issues.CreateComment(ctx, snooze.RepoOwner, snooze.RepoName, snooze.IssueID, comment)
	return err
}
