package main

import (
	"testing"
	"time"
)

func TestParseCommand(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		botCommand string
		wantHasCmd bool
		wantDate   string
	}{
		{
			name:       "Basic snooze",
			body:       "@snooze tomorrow",
			botCommand: "@snooze",
			wantHasCmd: true,
			wantDate:   "tomorrow",
		},
		{
			name:       "Snooze with extra text",
			body:       "This is a comment\n@snooze next week\nAnother line",
			botCommand: "@snooze",
			wantHasCmd: true,
			wantDate:   "next week",
		},
		{
			name:       "No snooze command",
			body:       "Just a regular comment",
			botCommand: "@snooze",
			wantHasCmd: false,
			wantDate:   "",
		},
		{
			name:       "Different bot command",
			body:       "@bot wake me up tomorrow",
			botCommand: "@bot",
			wantHasCmd: true,
			wantDate:   "wake me up tomorrow",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseCommand(tt.body, tt.botCommand)
			if got.HasCommand != tt.wantHasCmd {
				t.Errorf("ParseCommand() HasCommand = %v, want %v", got.HasCommand, tt.wantHasCmd)
			}
			if got.DateString != tt.wantDate {
				t.Errorf("ParseCommand() DateString = %v, want %v", got.DateString, tt.wantDate)
			}
		})
	}
}

func TestParseTargetTime(t *testing.T) {
	locNY, _ := time.LoadLocation("America/New_York")
	locUTC := time.UTC

	// Fixed "now" for testing: 2023-10-01 12:00:00 UTC
	now := time.Date(2023, 10, 1, 12, 0, 0, 0, locUTC)

	tests := []struct {
		name      string
		dateStr   string
		loc       *time.Location
		now       time.Time
		wantYear  int
		wantMonth time.Month
		wantDay   int
		wantHour  int
		wantErr   bool
	}{
		{
			name:      "Tomorrow defaults to 9 AM",
			dateStr:   "tomorrow",
			loc:       locUTC,
			now:       now,
			wantYear:  2023,
			wantMonth: 10,
			wantDay:   2,
			wantHour:  9,
			wantErr:   false,
		},
		{
			name:      "Specific date defaults to 9 AM",
			dateStr:   "Oct 5",
			loc:       locUTC,
			now:       now,
			wantYear:  2023,
			wantMonth: 10,
			wantDay:   5,
			wantHour:  9,
			wantErr:   false,
		},
		{
			name:      "Specific date and time",
			dateStr:   "Oct 5 at 3pm",
			loc:       locUTC,
			now:       now,
			wantYear:  2023,
			wantMonth: 10,
			wantDay:   5,
			wantHour:  15, // 3 PM
			wantErr:   false,
		},
		{
			name:      "Tomorrow defaults to 9 AM in NY",
			dateStr:   "tomorrow",
			loc:       locNY,
			now:       now, // 12 PM UTC is 8 AM NY time on Oct 1
			wantYear:  2023,
			wantMonth: 10,
			wantDay:   2, // Oct 2, 9 AM NY time
			wantHour:  9,
			wantErr:   false,
		},
		{
			name: "Today if 9 AM hasn't passed",
			// Let's set 'now' to 8 AM
			dateStr:   "today",
			loc:       locUTC,
			now:       time.Date(2023, 10, 1, 8, 0, 0, 0, locUTC),
			wantYear:  2023,
			wantMonth: 10,
			wantDay:   1,
			wantHour:  9,
			wantErr:   false,
		},
		{
			name: "Tomorrow if today's 9 AM has passed and no time given",
			// 'now' is 12 PM, so asking for "today" without time defaults to 9 AM, which passed, so it should bump to tomorrow
			dateStr:   "today",
			loc:       locUTC,
			now:       now,
			wantYear:  2023,
			wantMonth: 10,
			wantDay:   2,
			wantHour:  9,
			wantErr:   false,
		},
		{
			name:    "Invalid date string",
			dateStr: "jibberish",
			loc:     locUTC,
			now:     now,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseTargetTime(tt.dateStr, tt.loc, tt.now)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseTargetTime() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if got.Year() != tt.wantYear || got.Month() != tt.wantMonth || got.Day() != tt.wantDay || got.Hour() != tt.wantHour {
					t.Errorf("ParseTargetTime() got = %v (Year:%d, Month:%d, Day:%d, Hour:%d), want Year:%d, Month:%d, Day:%d, Hour:%d",
						got, got.Year(), got.Month(), got.Day(), got.Hour(), tt.wantYear, tt.wantMonth, tt.wantDay, tt.wantHour)
				}
			}
		})
	}
}
