package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/go-github/v62/github"
	"github.com/olebedev/when"
	"github.com/olebedev/when/rules/common"
	"github.com/olebedev/when/rules/en"
)

// SnoozeCommand represents a parsed snooze command
type SnoozeCommand struct {
	HasCommand bool
	DateString string
}

// ParseCommand extracts the snooze string from a comment body
func ParseCommand(body string, botCommand string) SnoozeCommand {
	lines := strings.Split(body, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, botCommand) {
			dateStr := strings.TrimSpace(strings.TrimPrefix(line, botCommand))
			return SnoozeCommand{
				HasCommand: true,
				DateString: dateStr,
			}
		}
	}
	return SnoozeCommand{HasCommand: false}
}

// GetUserLocation fetches the user's location from GitHub
func GetUserLocation(ctx context.Context, client *github.Client, username string) *time.Location {
	if client == nil {
		return time.UTC
	}

	user, _, err := client.Users.Get(ctx, username)
	if err != nil || user.Location == nil || *user.Location == "" {
		return time.UTC
	}

	// This is a simplified timezone resolution. A real implementation might need a mapping
	// of cities/countries to IANA timezones (e.g., using a geocoding service).
	// Here we try to parse it as a valid IANA location if possible, otherwise fallback to UTC.
	loc, err := time.LoadLocation(*user.Location)
	if err == nil {
		return loc
	}

	// Default to UTC if location cannot be parsed directly to a timezone
	return time.UTC
}

// ParseTargetTime parses the natural language date string into a time.Time based on location
func ParseTargetTime(dateStr string, loc *time.Location, now time.Time) (time.Time, error) {
	w := when.New(nil)
	w.Add(en.All...)
	w.Add(common.All...)

	// Parse in the context of 'now' in the specific location
	nowLocal := now.In(loc)
	r, err := w.Parse(dateStr, nowLocal)
	if err != nil {
		return time.Time{}, err
	}
	if r == nil {
		return time.Time{}, fmt.Errorf("could not parse date string: %s", dateStr)
	}

	// Check if time is provided in the input, if not default to 9 AM
	// This uses a simplistic heuristic: if "at" or a time format like "pm" isn't present,
	// we assume they only provided a date. A better approach might inspect the AST.
	lowerDateStr := strings.ToLower(dateStr)
	hasTime := strings.Contains(lowerDateStr, "at") ||
		strings.Contains(lowerDateStr, "am") ||
		strings.Contains(lowerDateStr, "pm") ||
		strings.Contains(lowerDateStr, ":")

	targetTime := r.Time
	if !hasTime {
		// Set to 9 AM local time
		targetTime = time.Date(targetTime.Year(), targetTime.Month(), targetTime.Day(), 9, 0, 0, 0, loc)
	}

	// Ensure the time is in the future
	if targetTime.Before(now) {
		// If the time has passed, try adding 24 hours
		if !hasTime {
			targetTime = targetTime.Add(24 * time.Hour)
		}
	}

	return targetTime, nil
}
