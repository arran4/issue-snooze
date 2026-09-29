package snoozebot

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/go-github/v62/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	app := &App{DB: db}

	require.NoError(t, InsertSnooze(db, "owner", "repo", 1, "user", time.Now().Add(-time.Hour), 1))
	now := time.Now()
	claimed, err := app.claimSnooze(1, now, "worker-1")
	require.NoError(t, err)
	assert.True(t, claimed)

	claimed, err = app.claimSnooze(1, now, "worker-2")
	require.NoError(t, err)
	assert.False(t, claimed)

	require.NoError(t, app.releaseSnooze(1, "worker-stale"))
	claimed, err = app.claimSnooze(1, now, "worker-2")
	require.NoError(t, err)
	assert.False(t, claimed, "a stale owner must not release another owner's claim")

	require.NoError(t, app.releaseSnooze(1, "worker-1"))
	claimed, err = app.claimSnooze(1, now, "worker-2")
	require.NoError(t, err)
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


func TestHandleRepositoryLifecycleEvents(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	_, err = db.Exec(`CREATE TABLE snoozes (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		repo_owner TEXT,
		repo_name TEXT,
		installation_id INTEGER
	)`)
	require.NoError(t, err)
	app := &App{DB: db}

	_, err = db.Exec(`INSERT INTO snoozes (repo_owner, repo_name, installation_id)
		VALUES ('oldowner', 'oldrepo', 10), ('oldowner', 'oldrepo', 20)`)
	require.NoError(t, err)

	renamed := "renamed"
	oldName := "oldrepo"
	err = app.handleRepositoryEvent(&github.RepositoryEvent{
		Action: &renamed,
		Installation: &github.Installation{ID: github.Int64(10)},
		Repo: &github.Repository{
			Name:  github.String("newrepo"),
			Owner: &github.User{Login: github.String("oldowner")},
		},
		Changes: &github.EditChange{
			Repo: &github.EditRepo{Name: &github.RepoName{From: &oldName}},
		},
	})
	require.NoError(t, err)

	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM snoozes
		WHERE repo_owner='oldowner' AND repo_name='newrepo' AND installation_id=10`).Scan(&count))
	assert.Equal(t, 1, count)
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM snoozes
		WHERE repo_owner='oldowner' AND repo_name='oldrepo' AND installation_id=20`).Scan(&count))
	assert.Equal(t, 1, count, "another installation must remain untouched")

	transferred := "transferred"
	oldOwner := "oldowner"
	err = app.handleRepositoryEvent(&github.RepositoryEvent{
		Action: &transferred,
		Installation: &github.Installation{ID: github.Int64(10)},
		Repo: &github.Repository{
			Name:  github.String("newrepo"),
			Owner: &github.User{Login: github.String("newowner")},
		},
		Changes: &github.EditChange{
			Owner: &github.EditOwner{OwnerInfo: &github.OwnerInfo{
				User: &github.User{Login: &oldOwner},
			}},
		},
	})
	require.NoError(t, err)
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM snoozes
		WHERE repo_owner='newowner' AND repo_name='newrepo' AND installation_id=10`).Scan(&count))
	assert.Equal(t, 1, count)

	err = app.handleRepositoryEvent(&github.RepositoryEvent{
		Action:       &transferred,
		Installation: &github.Installation{ID: github.Int64(10)},
		Repo: &github.Repository{
			Name:  github.String("newrepo"),
			Owner: &github.User{Login: github.String("newowner")},
		},
		Changes: &github.EditChange{},
	})
	assert.Error(t, err)

	targetRenamed := "renamed"
	oldAccount := "oldowner"
	err = app.handleInstallationTargetEvent(&github.InstallationTargetEvent{
		Action:       &targetRenamed,
		Installation: &github.Installation{ID: github.Int64(20)},
		Account:      &github.User{Login: github.String("renamedowner")},
		Changes: &github.InstallationChanges{
			Login: &github.InstallationLoginChange{From: &oldAccount},
		},
	})
	require.NoError(t, err)
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM snoozes
		WHERE repo_owner='renamedowner' AND repo_name='oldrepo' AND installation_id=20`).Scan(&count))
	assert.Equal(t, 1, count)
}

func writeTestPrivateKey(t *testing.T) string {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	keyFile := path.Join(t.TempDir(), "app.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
	})
	require.NoError(t, os.WriteFile(keyFile, pemBytes, 0o600))
	return keyFile
}

func TestAppTransportCachingAndEviction(t *testing.T) {
	config := Config{
		GitHubAppID:             123,
		GitHubAppPrivateKeyFile: writeTestPrivateKey(t),
		WebhookSecret:           "dummy",
	}
	app := &App{Config: config}

	client1, err := app.getClient(10)
	require.NoError(t, err)
	client2, err := app.getClient(10)
	require.NoError(t, err)
	assert.True(t, client1.Client().Transport == client2.Client().Transport)

	client3, err := app.getClient(20)
	require.NoError(t, err)
	assert.True(t, client1.Client().Transport != client3.Client().Transport)

	var wg sync.WaitGroup
	transports := make(chan http.RoundTripper, 8)
	errors := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client, err := app.getClient(30)
			if err != nil {
				errors <- err
				return
			}
			transports <- client.Client().Transport
		}()
	}
	wg.Wait()
	close(transports)
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	var first http.RoundTripper
	for transport := range transports {
		if first == nil {
			first = transport
			continue
		}
		assert.True(t, first == transport, "concurrent first access should reuse one transport")
	}

	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	_, err = db.Exec(`CREATE TABLE snoozes (id INTEGER PRIMARY KEY AUTOINCREMENT, installation_id INTEGER)`)
	require.NoError(t, err)
	app.DB = db
	deleted := "deleted"
	require.NoError(t, app.handleInstallationEvent(&github.InstallationEvent{
		Action:       &deleted,
		Installation: &github.Installation{ID: github.Int64(10)},
	}))
	client4, err := app.getClient(10)
	require.NoError(t, err)
	assert.True(t, client1.Client().Transport != client4.Client().Transport)

	restarted := &App{Config: config}
	restartedClient, err := restarted.getClient(10)
	require.NoError(t, err)
	assert.True(t, restartedClient.Client().Transport != client4.Client().Transport,
		"restart must reconstruct installation auth rather than persist token/transport state")
}

func webhookSignature(secret string, payload []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(payload)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestWebhookSignatureValidationMatrix(t *testing.T) {
	const secret = "test_secret"
	app := &App{Config: Config{GitHubAppID: 123, WebhookSecret: secret}}
	payload := []byte(`{"action":"opened"}`)

	request := func(signature string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(payload))
		req.Header.Set("X-GitHub-Event", "issues")
		req.Header.Set("X-GitHub-Delivery", "delivery-123")
		req.Header.Set("Content-Type", "application/json")
		if signature != "" {
			req.Header.Set("X-Hub-Signature-256", signature)
		}
		return req
	}

	rr := httptest.NewRecorder()
	app.handleWebhook(rr, request(webhookSignature(secret, payload)))
	assert.Equal(t, http.StatusOK, rr.Code)

	rr = httptest.NewRecorder()
	app.handleWebhook(rr, request("sha256=invalid"))
	assert.Equal(t, http.StatusBadRequest, rr.Code)

	rr = httptest.NewRecorder()
	app.handleWebhook(rr, request(""))
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestHandleWebhookInvalidAppConfig(t *testing.T) {
	app := &App{Config: Config{GitHubAppID: 123}}
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(nil))
	rr := httptest.NewRecorder()
	app.handleWebhook(rr, req)
	assert.Equal(t, http.StatusInternalServerError, rr.Code)
}

func TestRenewSnoozeClaim(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	_, err = db.Exec(`CREATE TABLE snoozes (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		locked_until DATETIME,
		claim_owner TEXT
	)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO snoozes (id, locked_until, claim_owner)
		VALUES (1, datetime('now', '+5 minutes'), 'worker-1')`)
	require.NoError(t, err)
	app := &App{DB: db}

	assert.True(t, app.renewSnoozeClaimOnce(1, "worker-1"))
	assert.False(t, app.renewSnoozeClaimOnce(1, "worker-stale"))

	_, err = db.Exec(`UPDATE snoozes SET claim_owner='worker-2' WHERE id=1`)
	require.NoError(t, err)
	cancelled := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go app.renewSnoozeClaimLoop(ctx, 1, "worker-1", func() { cancelled <- struct{}{} }, 5*time.Millisecond)

	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("ownership loss did not cancel in-flight work")
	}
}
