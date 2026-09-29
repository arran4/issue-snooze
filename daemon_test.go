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
	_ "github.com/mattn/go-sqlite3"
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
		t.Fatalf("failed to read %s: %v", testdataDir, err)
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
				switch {
				case f.Name == "env":
					for _, line := range strings.Split(string(f.Data), "\n") {
						line = strings.TrimSpace(line)
						if line == "" {
							continue
						}
						parts := strings.SplitN(line, "=", 2)
						if len(parts) == 2 {
							envVars[parts[0]] = parts[1]
						}
					}
				case f.Name == "expected.json":
					expectedJSON = f.Data
				case strings.HasPrefix(f.Name, "fs/"):
					vfs[strings.TrimPrefix(f.Name, "fs/")] = &fstest.MapFile{Data: append([]byte(nil), f.Data...)}
				}
			}

			oldLookupEnv := lookupEnv
			oldReadFile := readFile
			defer func() {
				lookupEnv = oldLookupEnv
				readFile = oldReadFile
			}()

			lookupEnv = func(key string) (string, bool) {
				val, ok := envVars[key]
				return val, ok
			}
			readFile = func(name string) ([]byte, error) {
				return fs.ReadFile(vfs, strings.TrimPrefix(name, "/"))
			}

			cfg := loadConfig()
			var expectedConfig Config
			if err := json.Unmarshal(expectedJSON, &expectedConfig); err != nil {
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
	defer func() { _ = db.Close() }()
	app := &App{DB: db}

	err = InsertSnooze(db, "owner", "repo", 1, "user", time.Now().Add(-time.Hour), 1)
	assert.NoError(t, err)

	now := time.Now()
	claimed, err := app.claimSnooze(1, now, "worker-1")
	assert.NoError(t, err)
	assert.True(t, claimed)

	claimed, err = app.claimSnooze(1, now, "worker-2")
	assert.NoError(t, err)
	assert.False(t, claimed)

	renewCtx, cancelRenew := context.WithTimeout(context.Background(), time.Second)
	defer cancelRenew()
	go app.renewSnoozeClaimLoop(renewCtx, 1, "worker-1", func() {}, time.Millisecond)
	time.Sleep(10 * time.Millisecond)

	err = app.releaseSnooze(1, "worker-1")
	assert.NoError(t, err)

	claimed, err = app.claimSnooze(1, now, "worker-2")
	assert.NoError(t, err)
	assert.True(t, claimed)
}

func TestIdempotentInsert(t *testing.T) {
	dbFile := "test_idempotent.db"
	defer func() { _ = os.Remove(dbFile) }()

	db, err := InitDB(dbFile)
	assert.NoError(t, err)
	defer func() { _ = db.Close() }()

	app := &App{DB: db}
	err = app.insertSnoozeIdempotent("delivery-123", "owner", "repo", 1, "user", time.Now(), 1)
	assert.NoError(t, err)

	var count int
	_ = db.QueryRow("SELECT COUNT(*) FROM snoozes").Scan(&count)
	assert.Equal(t, 1, count)
	_ = db.QueryRow("SELECT COUNT(*) FROM processed_deliveries").Scan(&count)
	assert.Equal(t, 1, count)

	err = app.insertSnoozeIdempotent("delivery-123", "owner", "repo", 2, "user", time.Now(), 1)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "UNIQUE constraint failed")
	_ = db.QueryRow("SELECT COUNT(*) FROM snoozes").Scan(&count)
	assert.Equal(t, 1, count)

	_, _ = db.Exec("DROP TABLE snoozes")
	err = app.insertSnoozeIdempotent("delivery-456", "owner", "repo", 1, "user", time.Now(), 1)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to insert snooze inside transaction")
	_ = db.QueryRow("SELECT COUNT(*) FROM processed_deliveries WHERE delivery_id = 'delivery-456'").Scan(&count)
	assert.Equal(t, 0, count)

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

	err = app.insertSnoozeIdempotent("delivery-456", "owner", "repo", 1, "user", time.Now(), 1)
	assert.NoError(t, err)
	_ = db.QueryRow("SELECT COUNT(*) FROM processed_deliveries WHERE delivery_id = 'delivery-456'").Scan(&count)
	assert.Equal(t, 1, count)
	_ = db.QueryRow("SELECT COUNT(*) FROM snoozes").Scan(&count)
	assert.Equal(t, 1, count)
}

func TestAppGetClient(t *testing.T) {
	app := &App{
		Config: Config{GitHubToken: "secret_pat"},
		Client: &github.Client{},
	}
	client, err := app.getClient(123)
	assert.NoError(t, err)
	assert.Equal(t, app.Client, client)

	app = &App{Config: Config{}}
	client, err = app.getClient(123)
	assert.Error(t, err)
	assert.Nil(t, client)

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

	for _, tc := range []struct {
		name string
		raw  string
		id   int64
	}{
		{name: "malformed", raw: "bad_value", id: -1},
		{name: "negative", raw: "-2", id: -2},
		{name: "zero", raw: "0", id: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{GitHubAppID: tc.id, GitHubAppIDRaw: tc.raw, GitHubToken: "secret_pat"}
			err := validateConfig(cfg)
			assert.Error(t, err)
			assert.Contains(t, err.Error(), "GITHUB_APP_ID must be a valid positive integer")
		})
	}
}

func TestHandleRepositoryEvent(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	assert.NoError(t, err)
	defer func() { _ = db.Close() }()
	app := &App{DB: db}
	_, err = db.Exec(`CREATE TABLE snoozes (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		repo_owner TEXT,
		repo_name TEXT,
		issue_id INTEGER,
		username TEXT,
		target_time DATETIME,
		installation_id INTEGER
	);`)
	assert.NoError(t, err)

	_, err = db.Exec(`INSERT INTO snoozes (repo_owner, repo_name, installation_id) VALUES
		('oldowner', 'oldrepo', 10),
		('oldowner', 'oldrepo', 20)`)
	assert.NoError(t, err)

	renameAction := "renamed"
	oldName := "oldrepo"
	e1 := &github.RepositoryEvent{
		Action:       &renameAction,
		Installation: &github.Installation{ID: github.Int64(10)},
		Repo: &github.Repository{
			Name:  github.String("newrepo"),
			Owner: &github.User{Login: github.String("oldowner")},
		},
		Changes: &github.EditChange{
			Repo: &github.EditRepo{Name: &github.RepoName{From: &oldName}},
		},
	}
	assert.NoError(t, app.handleRepositoryEvent(e1))

	var count int
	_ = db.QueryRow(`SELECT COUNT(*) FROM snoozes WHERE repo_owner = 'oldowner' AND repo_name = 'newrepo' AND installation_id = 10`).Scan(&count)
	assert.Equal(t, 1, count)
	_ = db.QueryRow(`SELECT COUNT(*) FROM snoozes WHERE repo_owner = 'oldowner' AND repo_name = 'oldrepo' AND installation_id = 20`).Scan(&count)
	assert.Equal(t, 1, count)

	transferAction := "transferred"
	oldOwnerLogin := "oldowner"
	e2 := &github.RepositoryEvent{
		Action:       &transferAction,
		Installation: &github.Installation{ID: github.Int64(10)},
		Repo: &github.Repository{
			Name:  github.String("newrepo"),
			Owner: &github.User{Login: github.String("newowner")},
		},
		Changes: &github.EditChange{
			Owner: &github.EditOwner{
				OwnerInfo: &github.OwnerInfo{User: &github.User{Login: &oldOwnerLogin}},
			},
		},
	}
	assert.NoError(t, app.handleRepositoryEvent(e2))
	_ = db.QueryRow(`SELECT COUNT(*) FROM snoozes WHERE repo_owner = 'newowner' AND repo_name = 'newrepo' AND installation_id = 10`).Scan(&count)
	assert.Equal(t, 1, count)

	missingOldOwner := &github.RepositoryEvent{
		Action:       &transferAction,
		Installation: &github.Installation{ID: github.Int64(10)},
		Repo: &github.Repository{
			Name:  github.String("newrepo"),
			Owner: &github.User{Login: github.String("newowner")},
		},
		Changes: &github.EditChange{},
	}
	err = app.handleRepositoryEvent(missingOldOwner)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "missing old owner identity")

	targetRenameAction := "renamed"
	oldAccountLogin := "oldowner"
	targetRename := &github.InstallationTargetEvent{
		Action:       &targetRenameAction,
		Installation: &github.Installation{ID: github.Int64(20)},
		Account:      &github.User{Login: github.String("newowner")},
		Changes: &github.InstallationChanges{
			Login: &github.InstallationLoginChange{From: &oldAccountLogin},
		},
	}
	assert.NoError(t, app.handleInstallationTargetEvent(targetRename))
	_ = db.QueryRow(`SELECT COUNT(*) FROM snoozes WHERE repo_owner = 'newowner' AND repo_name = 'oldrepo' AND installation_id = 20`).Scan(&count)
	assert.Equal(t, 1, count)

	missingTargetChanges := &github.InstallationTargetEvent{
		Action:       &targetRenameAction,
		Installation: &github.Installation{ID: github.Int64(20)},
		Account:      &github.User{Login: github.String("anotherowner")},
	}
	assert.Error(t, app.handleInstallationTargetEvent(missingTargetChanges))
}

func TestAppGetTransportCaching(t *testing.T) {
	keyFile := path.Join(t.TempDir(), "test_app_private_key.pem")
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	assert.NoError(t, err)
	privateKeyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
	})
	assert.NoError(t, os.WriteFile(keyFile, privateKeyPEM, 0600))

	app := &App{
		Config: Config{
			GitHubAppID:             123,
			GitHubAppPrivateKeyFile: keyFile,
			WebhookSecret:           "dummy",
		},
	}

	_, err = app.getClient(10)
	assert.NoError(t, err)
	transport10 := app.appTransports[10]
	assert.NotNil(t, transport10)

	_, err = app.getClient(10)
	assert.NoError(t, err)
	assert.True(t, transport10 == app.appTransports[10])

	_, err = app.getClient(20)
	assert.NoError(t, err)
	assert.True(t, transport10 != app.appTransports[20])

	db, err := sql.Open("sqlite3", ":memory:")
	assert.NoError(t, err)
	defer func() { _ = db.Close() }()
	app.DB = db
	_, err = db.Exec(`CREATE TABLE snoozes (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		repo_owner TEXT,
		repo_name TEXT,
		issue_id INTEGER,
		username TEXT,
		target_time DATETIME,
		installation_id INTEGER
	);`)
	assert.NoError(t, err)

	action := "deleted"
	event := &github.InstallationEvent{
		Action:       &action,
		Installation: &github.Installation{ID: github.Int64(10)},
	}
	assert.NoError(t, app.handleInstallationEvent(event))

	_, err = app.getClient(10)
	assert.NoError(t, err)
	assert.True(t, transport10 != app.appTransports[10])
	transportAfterEviction := app.appTransports[10]

	concurrentApp := &App{Config: app.Config}
	const workers = 8
	errCh := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, getErr := concurrentApp.getClient(30)
			errCh <- getErr
		}()
	}
	wg.Wait()
	close(errCh)
	for getErr := range errCh {
		assert.NoError(t, getErr)
	}
	assert.Len(t, concurrentApp.appTransports, 1)
	assert.NotNil(t, concurrentApp.appTransports[30])

	restartedApp := &App{Config: app.Config}
	_, err = restartedApp.getClient(10)
	assert.NoError(t, err)
	assert.Len(t, restartedApp.appTransports, 1)
	assert.True(t, transportAfterEviction != restartedApp.appTransports[10])
}

func TestWebhookSignatureValidationMatrix(t *testing.T) {
	app := &App{
		Config: Config{
			GitHubAppID:   123,
			WebhookSecret: "test_secret",
		},
	}
	payload := []byte(`{"action":"opened"}`)
	mac := hmac.New(sha256.New, []byte("test_secret"))
	_, _ = mac.Write(payload)
	validSig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewBuffer(payload))
	req.Header.Set("X-GitHub-Event", "issues")
	req.Header.Set("X-Hub-Signature-256", validSig)
	req.Header.Set("X-GitHub-Delivery", "delivery-123")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	app.handleWebhook(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code)

	req = httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewBuffer(payload))
	req.Header.Set("X-GitHub-Event", "issues")
	req.Header.Set("X-Hub-Signature-256", "sha256=invalid")
	req.Header.Set("X-GitHub-Delivery", "delivery-124")
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	app.handleWebhook(rr, req)
	assert.Equal(t, http.StatusBadRequest, rr.Code)

	req = httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewBuffer(payload))
	req.Header.Set("X-GitHub-Event", "issues")
	req.Header.Set("X-GitHub-Delivery", "delivery-125")
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	app.handleWebhook(rr, req)
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestHandleWebhookInvalidAppConfig(t *testing.T) {
	app := &App{Config: Config{GitHubAppID: 123}}
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewBuffer(nil))
	rr := httptest.NewRecorder()
	app.handleWebhook(rr, req)
	assert.Equal(t, http.StatusInternalServerError, rr.Code)
}

func TestRenewSnoozeClaim(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	assert.NoError(t, err)
	defer func() { _ = db.Close() }()
	app := &App{DB: db}
	_, err = db.Exec(`CREATE TABLE snoozes (id INTEGER PRIMARY KEY AUTOINCREMENT, locked_until DATETIME, claim_owner TEXT);`)
	assert.NoError(t, err)
	_, err = db.Exec(`INSERT INTO snoozes (id, locked_until, claim_owner) VALUES (1, datetime('now', '+5 minutes'), 'worker-1')`)
	assert.NoError(t, err)

	assert.True(t, app.renewSnoozeClaimOnce(1, "worker-1"))
	assert.False(t, app.renewSnoozeClaimOnce(1, "worker-stale"))

	_, err = db.Exec(`UPDATE snoozes SET claim_owner = 'worker-2' WHERE id = 1`)
	assert.NoError(t, err)
	cancelled := make(chan struct{}, 1)
	cancelWork := func() {
		select {
		case cancelled <- struct{}{}:
		default:
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go app.renewSnoozeClaimLoop(ctx, 1, "worker-1", cancelWork, time.Millisecond)
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("ownership loss did not cancel work")
	}
}
