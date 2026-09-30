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

func (osFS) Open(name string) (fs.File, error) { return os.Open(name) }

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
			envVars := map[string]string{}
			var expectedJSON []byte
			vfs := fstest.MapFS{}
			for _, f := range ar.Files {
				if f.Name == "env" {
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
				} else if f.Name == "expected.json" {
					expectedJSON = f.Data
				} else if strings.HasPrefix(f.Name, "fs/") {
					vfs[strings.TrimPrefix(f.Name, "fs/")] = &fstest.MapFile{Data: append([]byte(nil), f.Data...)}
				}
			}
			oldLookupEnv, oldReadFile := lookupEnv, readFile
			defer func() {
				lookupEnv = oldLookupEnv
				readFile = oldReadFile
			}()
			lookupEnv = func(key string) (string, bool) {
				v, ok := envVars[key]
				return v, ok
			}
			readFile = func(name string) ([]byte, error) { return fs.ReadFile(vfs, strings.TrimPrefix(name, "/")) }
			cfg := loadConfig()
			var expectedConfig Config
			if err = json.Unmarshal(expectedJSON, &expectedConfig); err != nil {
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
	app := &App{DB: db, claimLeaseDuration: time.Minute}
	assert.NoError(t, InsertSnooze(db, "owner", "repo", 1, "user", time.Now().Add(-time.Hour), 1))
	now := time.Now()
	claimed, err := app.claimSnooze(1, now, "worker-1")
	assert.NoError(t, err)
	assert.True(t, claimed)
	claimed, err = app.claimSnooze(1, now, "worker-2")
	assert.NoError(t, err)
	assert.False(t, claimed)
	owned, err := app.renewSnoozeClaimOnce(1, "worker-1", now.Add(10*time.Second))
	assert.NoError(t, err)
	assert.True(t, owned)
	released, err := app.releaseSnooze(1, "worker-1")
	assert.NoError(t, err)
	assert.True(t, released)
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
	assert.NoError(t, app.insertSnoozeIdempotent("delivery-123", "owner", "repo", 1, "user", time.Now(), 1))
	var count int
	_ = db.QueryRow("SELECT COUNT(*) FROM snoozes").Scan(&count)
	assert.Equal(t, 1, count)
	_ = db.QueryRow("SELECT COUNT(*) FROM processed_deliveries").Scan(&count)
	assert.Equal(t, 1, count)
	err = app.insertSnoozeIdempotent("delivery-123", "owner", "repo", 2, "user", time.Now(), 1)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "UNIQUE constraint failed")
	_, _ = db.Exec("DROP TABLE snoozes")
	err = app.insertSnoozeIdempotent("delivery-456", "owner", "repo", 1, "user", time.Now(), 1)
	assert.Error(t, err)
	_ = db.QueryRow("SELECT COUNT(*) FROM processed_deliveries WHERE delivery_id = 'delivery-456'").Scan(&count)
	assert.Equal(t, 0, count)
	_, _ = db.Exec(`CREATE TABLE snoozes (id INTEGER PRIMARY KEY AUTOINCREMENT, repo_owner TEXT NOT NULL, repo_name TEXT NOT NULL, issue_id INTEGER NOT NULL, username TEXT NOT NULL, target_time DATETIME NOT NULL, installation_id INTEGER DEFAULT 0, locked_until DATETIME DEFAULT NULL, claim_owner TEXT DEFAULT NULL)`)
	assert.NoError(t, app.insertSnoozeIdempotent("delivery-456", "owner", "repo", 1, "user", time.Now(), 1))
	_ = db.QueryRow("SELECT COUNT(*) FROM snoozes").Scan(&count)
	assert.Equal(t, 1, count)
}

func TestAppGetClient(t *testing.T) {
	app := &App{Config: Config{GitHubToken: "secret_pat"}, Client: &github.Client{}}
	client, err := app.getClient(123)
	assert.NoError(t, err)
	assert.Equal(t, app.Client, client)
	app = &App{Config: Config{}}
	client, err = app.getClient(123)
	assert.Error(t, err)
	assert.Nil(t, client)
	app = &App{Config: Config{GitHubAppID: 1, GitHubAppPrivateKeyFile: "fake.pem", WebhookSecret: "secret"}, Client: &github.Client{}}
	client, err = app.getClient(0)
	assert.Error(t, err)
	assert.Nil(t, client)
	for _, raw := range []string{"bad_value", "-2", "0"} {
		app = &App{Config: Config{GitHubAppIDRaw: raw, GitHubToken: "secret_pat"}}
		if raw == "-2" {
			app.Config.GitHubAppID = -2
		}
		if raw == "bad_value" {
			app.Config.GitHubAppID = -1
		}
		err = validateConfig(app.Config)
		assert.Error(t, err)
	}
}
