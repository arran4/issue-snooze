package snoozebot

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/go-github/v62/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetUserLocationHTTPClassification(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		wantUTC    bool
		wantErr    bool
	}{
		{name: "not found falls back", status: http.StatusNotFound, wantUTC: true},
		{name: "unauthorized errors", status: http.StatusUnauthorized, wantErr: true},
		{name: "forbidden errors", status: http.StatusForbidden, wantErr: true},
		{name: "rate limited errors", status: http.StatusTooManyRequests, wantErr: true},
		{name: "server failure errors", status: http.StatusInternalServerError, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, closeServer := testGitHubClient(t, tc.status)
			defer closeServer()
			loc, err := GetUserLocation(context.Background(), client, "alice")
			if tc.wantErr {
				assert.Error(t, err)
				assert.Nil(t, loc)
				return
			}
			require.NoError(t, err)
			if tc.wantUTC {
				assert.Equal(t, time.UTC, loc)
			}
		})
	}
}

func TestGetUserLocationMissingAndInvalidLocationFallBackUTC(t *testing.T) {
	for _, locationJSON := range []string{"null", `"Not/A_Real_Zone"`} {
		t.Run(locationJSON, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"login":"alice","location":%s}`, locationJSON)
			}))
			defer server.Close()
			baseURL, err := url.Parse(server.URL + "/")
			require.NoError(t, err)
			client := github.NewClient(server.Client())
			client.BaseURL = baseURL
			client.UploadURL = baseURL

			loc, err := GetUserLocation(context.Background(), client, "alice")
			require.NoError(t, err)
			assert.Equal(t, time.UTC, loc)
		})
	}
}
