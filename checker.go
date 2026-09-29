package snoozebot

import (
	"context"
	"fmt"
	"log"
	"net/http"
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
		ownerToken := fmt.Sprintf("worker-%d-%d", snooze.ID, currentNow.UnixNano())
		claimed, err := a.claimSnooze(snooze.ID, currentNow, ownerToken)
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

		err = a.PostSnoozeReply(ctx, snooze, ownerToken)
		if err != nil {
			log.Printf("Failed to post snooze reply for ID %d: %v. Releasing claim for retry.", snooze.ID, err)
			// Release the claim so it can be retried later
			_ = a.releaseSnooze(snooze.ID, ownerToken)
			continue
		}

		// Successful post removes/completes it
		err = a.deleteSnoozeIfOwned(snooze.ID, ownerToken)
		if err != nil {
			log.Printf("Failed to delete processed snooze ID %d: %v", snooze.ID, err)
		}
	}
}

func (a *App) claimSnooze(id int, now time.Time, ownerToken string) (bool, error) {
	// Lock for 5 minutes initially with a specific owner token
	lockedUntil := now.Add(5 * time.Minute).UTC().Format(time.RFC3339)
	res, err := a.DB.Exec(`UPDATE snoozes SET locked_until = ?, claim_owner = ? WHERE id = ? AND (locked_until IS NULL OR locked_until < ?)`, lockedUntil, ownerToken, id, now.UTC().Format(time.RFC3339))
	if err != nil {
		return false, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}

func (a *App) renewSnoozeClaimLoop(ctx context.Context, id int, ownerToken string, cancelWork context.CancelFunc, tickDuration time.Duration) {
	ticker := time.NewTicker(tickDuration)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !a.renewSnoozeClaimOnce(id, ownerToken) {
				cancelWork()
				return
			}
		}
	}
}

func (a *App) renewSnoozeClaimOnce(id int, ownerToken string) bool {
	lockedUntil := time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)
	res, err := a.DB.Exec(`UPDATE snoozes SET locked_until = ? WHERE id = ? AND claim_owner = ?`, lockedUntil, id, ownerToken)
	if err != nil {
		log.Printf("Failed to renew claim for snooze ID %d: %v", id, err)
		return false
	}
	rows, err := res.RowsAffected()
	if err != nil {
		log.Printf("Failed to inspect renewed claim for snooze ID %d: %v", id, err)
		return false
	}
	if rows == 0 {
		log.Printf("Failed to renew claim for snooze ID %d: no longer owned by %s", id, ownerToken)
		return false
	}
	return true
}

func (a *App) releaseSnooze(id int, ownerToken string) error {
	// Release only if we are still the owner
	_, err := a.DB.Exec(`UPDATE snoozes SET locked_until = NULL, claim_owner = NULL WHERE id = ? AND claim_owner = ?`, id, ownerToken)
	return err
}

func (a *App) deleteSnoozeIfOwned(id int, ownerToken string) error {
	res, err := a.DB.Exec(`DELETE FROM snoozes WHERE id = ? AND claim_owner = ?`, id, ownerToken)
	if err != nil {
		return err
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("failed to delete snooze %d: no longer owned by %s", id, ownerToken)
	}
	return nil
}

// PostSnoozeReply posts a comment to the GitHub issue tagging the user.
func (a *App) PostSnoozeReply(ctx context.Context, snooze SnoozeRecord, ownerToken string) error {
	return a.postSnoozeReplyWithRenewInterval(ctx, snooze, ownerToken, 2*time.Minute)
}

func (a *App) postSnoozeReplyWithRenewInterval(ctx context.Context, snooze SnoozeRecord, ownerToken string, renewInterval time.Duration) error {
	workCtx, cancelWork := context.WithCancel(ctx)
	defer cancelWork()

	renewCtx, cancelRenew := context.WithCancel(ctx)
	defer cancelRenew()
	go a.renewSnoozeClaimLoop(renewCtx, snooze.ID, ownerToken, cancelWork, renewInterval)

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

	_, resp, err := client.Issues.CreateComment(workCtx, snooze.RepoOwner, snooze.RepoName, snooze.IssueID, comment)
	if err != nil && resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
		a.evictInstallationTransport(snooze.InstallationID)
	}
	return err
}
