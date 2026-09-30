package snoozebot

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/go-github/v62/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseCommand(t *testing.T) {
	tests := []struct {
		name, body, botCommand, wantDate string
		wantHasCmd                       bool
	}{
		{"Basic snooze", "@snooze tomorrow", "@snooze", "tomorrow", true},
		{"Snooze with extra text", "This is a comment\n@snooze next week\nAnother line", "@snooze", "next week", true},
		{"No snooze command", "Just a regular comment", "@snooze", "", false},
		{"Different bot command", "@bot wake me up tomorrow", "@bot", "wake me up tomorrow", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseCommand(tt.body, tt.botCommand)
			assert.Equal(t, tt.wantHasCmd, got.HasCommand)
			assert.Equal(t, tt.wantDate, got.DateString)
		})
	}
}

func TestParseTargetTime(t *testing.T) {
	locNY, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	utc := time.UTC
	now := time.Date(2023, 10, 1, 12, 0, 0, 0, utc)
	tests := []struct {
		name, dateStr string
		loc           *time.Location
		now           time.Time
		year          int
		month         time.Month
		day, hour     int
		wantErr       bool
	}{
		{"Tomorrow defaults to 9 AM", "tomorrow", utc, now, 2023, 10, 2, 9, false},
		{"Specific date defaults to 9 AM", "Oct 5", utc, now, 2023, 10, 5, 9, false},
		{"Specific date and time", "Oct 5 at 3pm", utc, now, 2023, 10, 5, 15, false},
		{"Tomorrow defaults to 9 AM in NY", "tomorrow", locNY, now, 2023, 10, 2, 9, false},
		{"Invalid date string", "jibberish", utc, now, 0, 0, 0, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseTargetTime(tt.dateStr, tt.loc, tt.now)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.year, got.Year())
			assert.Equal(t, tt.month, got.Month())
			assert.Equal(t, tt.day, got.Day())
			assert.Equal(t, tt.hour, got.Hour())
		})
	}
}

func TestGetUserLocationHTTPPolicy(t *testing.T) {
	tests := []struct {
		name             string
		status           int
		body             string
		wantUTC, wantErr bool
		wantStatus       int
	}{
		{"valid timezone", 200, `{"location":"Australia/Melbourne"}`, false, false, 0},
		{"missing timezone", 200, `{}`, true, false, 0},
		{"invalid timezone", 200, `{"location":"Melbourne, Australia"}`, true, false, 0},
		{"user unavailable", 404, `{}`, true, false, 0},
		{"unauthorized", 401, `{}`, false, true, 401},
		{"forbidden", 403, `{}`, false, true, 403},
		{"rate limited", 429, `{}`, false, true, 429},
		{"server error", 500, `{}`, false, true, 500},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/users/alice", r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()
			client := github.NewClient(srv.Client())
			base, err := url.Parse(srv.URL + "/")
			require.NoError(t, err)
			client.BaseURL = base
			loc, err := GetUserLocation(context.Background(), client, "alice")
			if tt.wantErr {
				require.Error(t, err)
				var se *GitHubStatusError
				require.True(t, errors.As(err, &se))
				assert.Equal(t, tt.wantStatus, se.StatusCode)
				return
			}
			require.NoError(t, err)
			if tt.wantUTC {
				assert.Equal(t, time.UTC, loc)
			} else {
				assert.Equal(t, "Australia/Melbourne", loc.String())
			}
		})
	}
}
