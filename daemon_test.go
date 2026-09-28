package snoozebot

import (
	"encoding/json"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/go-github/v62/github"
	"github.com/stretchr/testify/assert"
	"golang.org/x/tools/txtar"
)

type osFS struct{}

func (osFS) Open(name string) (fs.File, error) {
	return os.Open(name)
}

func TestConfigTxtar(t *testing.T) {
	testdataDir := "testdata/config"
	entries, err := fs.ReadDir(osFS{}, testdataDir)
	if err != nil {
		t.Fatalf("failed to read testdata dir: %v", err)
	}

	var cases []string
	for _, d := range entries {
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".txtar") {
			cases = append(cases, path.Join(testdataDir, d.Name()))
		}
	}
	sort.Strings(cases)

	for _, tc := range cases {
		tc := tc
		t.Run(strings.TrimSuffix(path.Base(tc), ".txtar"), func(t *testing.T) {
			raw, err := fs.ReadFile(osFS{}, tc)
			if err != nil {
				t.Fatalf("failed to read %s: %v", tc, err)
			}
			ar := txtar.Parse(raw)

			envVars := make(map[string]string)
			var expectedJSON []byte
			vfs := fstest.MapFS{}

			for _, f := range ar.Files {
				if f.Name == "env" {
					lines := strings.Split(string(f.Data), "\n")
					for _, line := range lines {
						line = strings.TrimSpace(line)
						if line == "" {
							continue
						}
						parts := strings.SplitN(line, "=", 2)
						if len(parts) == 2 {
							envVars[parts[0]] = parts[1]
						}
					}
				} else if f.Name == "expected.json" {
					expectedJSON = f.Data
				} else if strings.HasPrefix(f.Name, "fs/") {
					vfs[strings.TrimPrefix(f.Name, "fs/")] = &fstest.MapFile{Data: append([]byte(nil), f.Data...)}
				}
			}

			// Save old lookup functions and restore them later
			oldLookupEnv := lookupEnv
			oldReadFile := readFile
			defer func() {
				lookupEnv = oldLookupEnv
				readFile = oldReadFile
			}()

			// Mock environment variables
			lookupEnv = func(key string) (string, bool) {
				val, ok := envVars[key]
				return val, ok
			}

			// Mock file reader
			readFile = func(name string) ([]byte, error) {
				return fs.ReadFile(vfs, strings.TrimPrefix(name, "/"))
			}

			cfg := loadConfig()

			var expectedConfig Config
			err = json.Unmarshal(expectedJSON, &expectedConfig)
			if err != nil {
				t.Fatalf("failed to parse expected.json: %v", err)
			}

			assert.Equal(t, expectedConfig, cfg)
			err = validateConfig(cfg)
			if expectedConfig.GitHubToken == "" && expectedConfig.GitHubAppID == 0 {
				assert.Error(t, err)
			} else if expectedConfig.GitHubAppID > 0 && (expectedConfig.WebhookSecret == "" || expectedConfig.GitHubAppPrivateKeyFile == "") {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestClaimSnooze(t *testing.T) {
	dbFile := "test_claim.db"
	defer func() { _ = os.Remove(dbFile) }()
	db, err := InitDB(dbFile)
	assert.NoError(t, err)
	defer db.Close()
	app := &App{DB: db}

	err = InsertSnooze(db, "owner", "repo", 1, "user", time.Now().Add(-time.Hour), 1)
	assert.NoError(t, err)

	now := time.Now()
	// Claim 1
	claimed, err := app.claimSnooze(1, now, "worker-1")
	assert.NoError(t, err)
	assert.True(t, claimed)

	// Claim 2 fails
	claimed, err = app.claimSnooze(1, now, "worker-2")
	assert.NoError(t, err)
	assert.False(t, claimed)

	// Renew 1
	res, err := db.Exec(`UPDATE snoozes SET locked_until = ? WHERE id = ? AND claim_owner = ?`, now.Add(5*time.Minute).UTC().Format(time.RFC3339), 1, "worker-1")
	assert.NoError(t, err)
	rows, _ := res.RowsAffected()
	assert.Equal(t, int64(1), rows)

	// Release 1
	err = app.releaseSnooze(1, "worker-1")
	assert.NoError(t, err)

	// Claim 2 now succeeds
	claimed, err = app.claimSnooze(1, now, "worker-2")
	assert.NoError(t, err)
	assert.True(t, claimed)
}

func TestIdempotentInsert(t *testing.T) {
	dbFile := "test_idempotent.db"
	defer func() {
		_ = os.Remove(dbFile)
	}()

	db, err := InitDB(dbFile)
	assert.NoError(t, err)
	defer func() { _ = db.Close() }()

	app := &App{DB: db}

	// 1. First insert should succeed
	err = app.insertSnoozeIdempotent("delivery-123", "owner", "repo", 1, "user", time.Now(), 1)
	assert.NoError(t, err)

	var count int
	_ = db.QueryRow("SELECT COUNT(*) FROM snoozes").Scan(&count)
	assert.Equal(t, 1, count)

	_ = db.QueryRow("SELECT COUNT(*) FROM processed_deliveries").Scan(&count)
	assert.Equal(t, 1, count)

	// 2. Second insert with same delivery ID should fail with unique constraint error
	err = app.insertSnoozeIdempotent("delivery-123", "owner", "repo", 2, "user", time.Now(), 1)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "UNIQUE constraint failed")

	// No new snoozes should be inserted
	_ = db.QueryRow("SELECT COUNT(*) FROM snoozes").Scan(&count)
	assert.Equal(t, 1, count)

	// 3. Rollback scenario: a failed snooze insert inside transaction shouldn't block future delivery
	// We can trigger an error intentionally by passing an invalid NOT NULL constraint violation (e.g., if we could pass NULL).
	// Since we can't do that easily without refactoring the signature, we'll manually drop a table temporarily to force an error.

	// Drop snoozes temporarily so insert fails
	_, _ = db.Exec("DROP TABLE snoozes")

	err = app.insertSnoozeIdempotent("delivery-456", "owner", "repo", 1, "user", time.Now(), 1)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to insert snooze inside transaction")

	// Check that the delivery marker was rolled back
	_ = db.QueryRow("SELECT COUNT(*) FROM processed_deliveries WHERE delivery_id = 'delivery-456'").Scan(&count)
	assert.Equal(t, 0, count)

	// Recreate table
	query := `
	CREATE TABLE snoozes (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		repo_owner TEXT NOT NULL,
		repo_name TEXT NOT NULL,
		issue_id INTEGER NOT NULL,
		username TEXT NOT NULL,
		target_time DATETIME NOT NULL,
		installation_id INTEGER DEFAULT 0,
		locked_until DATETIME DEFAULT NULL,
			claim_owner TEXT DEFAULT NULL
	);
	`
	_, _ = db.Exec(query)

	// Try successful retry
	err = app.insertSnoozeIdempotent("delivery-456", "owner", "repo", 1, "user", time.Now(), 1)
	assert.NoError(t, err)

	_ = db.QueryRow("SELECT COUNT(*) FROM processed_deliveries WHERE delivery_id = 'delivery-456'").Scan(&count)
	assert.Equal(t, 1, count)

	_ = db.QueryRow("SELECT COUNT(*) FROM snoozes").Scan(&count)
	assert.Equal(t, 1, count)
}

func TestAppGetClient(t *testing.T) {
	// 1. Fallback to PAT if App not configured but PAT is configured
	app := &App{
		Config: Config{
			GitHubToken: "secret_pat",
		},
		Client: &github.Client{},
	}
	client, err := app.getClient(123)
	assert.NoError(t, err)
	assert.Equal(t, app.Client, client)

	// 2. Error if nothing configured
	app = &App{
		Config: Config{},
		Client: nil,
	}
	client, err = app.getClient(123)
	assert.Error(t, err)
	assert.Nil(t, client)

	// 3. GitHub App mode fails if installationID is 0 rather than falling back to PAT silently
	app = &App{
		Config: Config{
			GitHubAppID:             1,
			GitHubAppPrivateKeyFile: "fake.pem",
			WebhookSecret:           "secret",
		},
		Client: &github.Client{},
	}
	client, err = app.getClient(0)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "GitHub App mode requires a valid installation ID")
	assert.Nil(t, client)

	// 4. Test validation logic blocks malformed App ID
	app = &App{
		Config: Config{
			GitHubAppID:    -1,
			GitHubAppIDRaw: "bad_value",
			GitHubToken:    "secret_pat",
		},
	}
	err = validateConfig(app.Config)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "GITHUB_APP_ID must be a valid positive integer")

	// 6. Test validation blocks explicit negative App ID -2
	app = &App{
		Config: Config{
			GitHubAppID:    -2,
			GitHubAppIDRaw: "-2",
			GitHubToken:    "secret_pat",
		},
	}
	err = validateConfig(app.Config)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "GITHUB_APP_ID must be a valid positive integer")

	// 5. Test validation blocks explicit 0 or negative App ID
	app = &App{
		Config: Config{
			GitHubAppID:    0,
			GitHubAppIDRaw: "0",
			GitHubToken:    "secret_pat",
		},
	}
	err = validateConfig(app.Config)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "GITHUB_APP_ID must be a valid positive integer")
}
