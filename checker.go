package snoozebot

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/google/go-github/v62/github"
)

const (
	defaultClaimLeaseDuration = 5 * time.Minute
	defaultClaimRenewInterval = 2 * time.Minute
)

func (a *App) claimLease() time.Duration {
	if a.claimLeaseDuration > 0 {
		return a.claimLeaseDuration
	}
	return defaultClaimLeaseDuration
}

func (a *App) claimRenewEvery() time.Duration {
	if a.claimRenewInterval > 0 {
		return a.claimRenewInterval
	}
	return defaultClaimRenewInterval
}

// StartBackgroundChecker starts a goroutine that checks immediately and then periodically.
func (a *App) StartBackgroundChecker(ctx context.Context, interval time.Duration) {
	go func() {
		a.CheckExpiredSnoozes(ctx)

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				a.CheckExpiredSnoozes(ctx)
			}
		}
	}()
}

// CheckExpiredSnoozes queries the database for expired snoozes and posts comments.
func (a *App) CheckExpiredSnoozes(ctx context.Context) {
	expired, err := GetExpiredSnoozes(a.DB, time.Now())
	if err != nil {
		log.Printf("Error getting expired snoozes: %v", err)
		return
	}

	for _, snooze := range expired {
		currentNow := time.Now()
		ownerToken := fmt.Sprintf("worker-%d-%d", snooze.ID, currentNow.UnixNano())
		claimed, err := a.claimSnooze(snooze.ID, currentNow, ownerToken)
		if err != nil {
			log.Printf("Failed to attempt claiming snooze ID %d: %v", snooze.ID, err)
			continue
		}
		if !claimed {
			continue
		}

		log.Printf("Processing expired snooze for @%s on %s/%s#%d", snooze.Username, snooze.RepoOwner, snooze.RepoName, snooze.IssueID)

		if err := a.PostSnoozeReply(ctx, snooze, ownerToken); err != nil {
			log.Printf("Failed to post snooze reply for ID %d: %v. Releasing claim for retry.", snooze.ID, err)
			if released, releaseErr := a.releaseSnooze(snooze.ID, ownerToken); releaseErr != nil {
				log.Printf("Failed to release snooze claim for ID %d: %v", snooze.ID, releaseErr)
			} else if !released {
				log.Printf("Snooze ID %d was no longer owned while releasing after failure", snooze.ID)
			}
			continue
		}

		if err := a.deleteSnoozeIfOwned(snooze.ID, ownerToken); err != nil {
			// A remote GitHub post may already have succeeded. Leaving the row allows retry,
			// which is why delivery is documented as at-least-once rather than exactly-once.
			log.Printf("Failed to delete processed snooze ID %d: %v", snooze.ID, err)
		}
	}
}

func (a *App) claimSnooze(id int, now time.Time, ownerToken string) (bool, error) {
	lockedUntil := now.Add(a.claimLease()).UTC().Format(time.RFC3339Nano)
	res, err := a.DB.Exec(`UPDATE snoozes
		SET locked_until = ?, claim_owner = ?
		WHERE id = ? AND (locked_until IS NULL OR locked_until < ?)`, lockedUntil, ownerToken, id, now.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return false, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}

func (a *App) renewSnoozeClaimOnce(id int, ownerToken string, now time.Time) (bool, error) {
	lockedUntil := now.Add(a.claimLease()).UTC().Format(time.RFC3339Nano)
	res, err := a.DB.Exec(`UPDATE snoozes SET locked_until = ? WHERE id = ? AND claim_owner = ?`, lockedUntil, id, ownerToken)
	if err != nil {
		return false, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}

func (a *App) renewSnoozeClaimLoop(ctx context.Context, id int, ownerToken string, cancelWork context.CancelFunc) {
	ticker := time.NewTicker(a.claimRenewEvery())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			owned, err := a.renewSnoozeClaimOnce(id, ownerToken, time.Now())
			if err != nil {
				log.Printf("Failed to renew claim for snooze ID %d: %v. Cancelling work.", id, err)
				cancelWork()
				return
			}
			if !owned {
				log.Printf("Failed to renew claim for snooze ID %d: no longer owned by %s. Cancelling work.", id, ownerToken)
				cancelWork()
				return
			}
		}
	}
}

func (a *App) releaseSnooze(id int, ownerToken string) (bool, error) {
	res, err := a.DB.Exec(`UPDATE snoozes SET locked_until = NULL, claim_owner = NULL WHERE id = ? AND claim_owner = ?`, id, ownerToken)
	if err != nil {
		return false, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}

func (a *App) deleteSnoozeIfOwned(id int, ownerToken string) error {
	res, err := a.DB.Exec(`DELETE FROM snoozes WHERE id = ? AND claim_owner = ?`, id, ownerToken)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return fmt.Errorf("failed to delete snooze %d: no longer owned by %s", id, ownerToken)
	}
	return nil
}

// PostSnoozeReply posts a comment to the GitHub issue tagging the user.
func (a *App) PostSnoozeReply(ctx context.Context, snooze SnoozeRecord, ownerToken string) error {
	workCtx, cancelWork := context.WithCancel(ctx)
	defer cancelWork()

	renewCtx, cancelRenew := context.WithCancel(ctx)
	defer cancelRenew()
	go a.renewSnoozeClaimLoop(renewCtx, snooze.ID, ownerToken, cancelWork)

	client, err := a.getClient(snooze.InstallationID)
	if err != nil {
		return fmt.Errorf("could not get GitHub client for reply: %w", err)
	}
	if client == nil {
		return fmt.Errorf("GitHub client is not initialized")
	}

	body := fmt.Sprintf("Hey @%s, your snooze is over!", snooze.Username)
	comment := &github.IssueComment{Body: &body}

	_, resp, err := client.Issues.CreateComment(workCtx, snooze.RepoOwner, snooze.RepoName, snooze.IssueID, comment)
	if err != nil && resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
		// Permission/auth failures preserve the reminder for retry but force fresh installation auth.
		a.evictInstallationTransport(snooze.InstallationID)
	}
	return err
}
