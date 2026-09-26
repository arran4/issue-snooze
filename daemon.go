package snoozebot

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v62/github"
)

// Config holds the configuration for the bot
type Config struct {
	GitHubToken             string
	GitHubAppID             int64
	GitHubAppPrivateKeyFile string
	WebhookSecret           string
	BotCommand              string
	Port                    string
	DatabaseFile            string
}

// loadConfig loads configuration from environment variables or Docker secrets
func loadConfig() Config {
	appIDStr := getEnv("GITHUB_APP_ID", "")
	var appID int64
	if appIDStr != "" {
		if id, err := strconv.ParseInt(appIDStr, 10, 64); err == nil {
			appID = id
		} else {
			log.Printf("Invalid GITHUB_APP_ID %q: %v", appIDStr, err)
		}
	}

	cfg := Config{
		GitHubToken:             getEnvOrSecret("GITHUB_TOKEN_FILE", "GITHUB_TOKEN", ""),
		GitHubAppID:             appID,
		GitHubAppPrivateKeyFile: getEnv("GITHUB_APP_PRIVATE_KEY_FILE", ""),
		WebhookSecret:           getEnvOrSecret("WEBHOOK_SECRET_FILE", "WEBHOOK_SECRET", ""),
		BotCommand:              getEnv("BOT_COMMAND", "@snooze"),
		Port:                    getEnv("PORT", "8080"),
		DatabaseFile:            getEnv("DATABASE_FILE", "snooze.db"),
	}
	return cfg
}

// Make OS interactions mockable for tests
var (
	lookupEnv = os.LookupEnv
	readFile  = os.ReadFile
)

func getEnv(key, fallback string) string {
	if value, exists := lookupEnv(key); exists {
		return value
	}
	return fallback
}

func getEnvOrSecret(fileKey, envKey, fallback string) string {
	if filePath, exists := lookupEnv(fileKey); exists {
		content, err := readFile(filePath)
		if err == nil {
			return strings.TrimSpace(string(content))
		}
		log.Printf("Failed to read secret file %s: %v", filePath, err)
	}
	if value, exists := lookupEnv(envKey); exists {
		return value
	}
	return fallback
}

// App holds the application state
type App struct {
	Config Config
	Client *github.Client // Fallback PAT client
	DB     *sql.DB
}

// getClient returns a GitHub client authenticated for the specific installation if possible,
// falling back to the PAT client otherwise.
func (a *App) getClient(installationID int64) (*github.Client, error) {
	if a.Config.GitHubAppID > 0 && a.Config.GitHubAppPrivateKeyFile != "" && installationID > 0 {
		itr, err := ghinstallation.NewKeyFromFile(http.DefaultTransport, a.Config.GitHubAppID, installationID, a.Config.GitHubAppPrivateKeyFile)
		if err != nil {
			return nil, fmt.Errorf("failed to create ghinstallation transport: %w", err)
		}
		return github.NewClient(&http.Client{Transport: itr}), nil
	}

	if a.Client != nil {
		return a.Client, nil
	}

	return nil, fmt.Errorf("no GitHub client available (PAT not set and GitHub App not fully configured)")
}

func (a *App) handleWebhook(w http.ResponseWriter, r *http.Request) {
	var payload []byte
	var err error

	// Require webhook secret for GitHub App mode
	if a.Config.GitHubAppID > 0 && a.Config.WebhookSecret == "" {
		log.Printf("Webhook secret is required when running as a GitHub App")
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	if a.Config.WebhookSecret != "" {
		payload, err = github.ValidatePayload(r, []byte(a.Config.WebhookSecret))
	} else {
		payload, err = github.ValidatePayload(r, nil)
	}

	if err != nil {
		log.Printf("Error validating webhook payload: %v", err)
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	event, err := github.ParseWebHook(github.WebHookType(r), payload)
	if err != nil {
		log.Printf("Error parsing webhook: %v", err)
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	switch e := event.(type) {
	case *github.IssueCommentEvent:
		a.handleIssueComment(e)
	default:
		// Not an issue comment event, ignore
	}
}

func (a *App) handleIssueComment(e *github.IssueCommentEvent) {
	if e.Action != nil && (*e.Action == "deleted" || *e.Action == "edited") {
		return
	}

	body := e.Comment.GetBody()
	cmd := ParseCommand(body, a.Config.BotCommand)
	if !cmd.HasCommand {
		return
	}

	log.Printf("Received snooze command on repo %s for issue %d", e.Repo.GetFullName(), e.Issue.GetNumber())

	var installationID int64
	if e.Installation != nil {
		installationID = e.Installation.GetID()
	}

	ctx := context.Background()
	username := e.Sender.GetLogin()

	// Try to get an authenticated client for location lookup
	client, err := a.getClient(installationID)
	if err != nil {
		log.Printf("Failed to get client for location lookup: %v", err)
		// Fallback to UTC if we can't get a client
		loc := time.UTC
		now := time.Now()
		targetTime, err := ParseTargetTime(cmd.DateString, loc, now)
		if err != nil {
			log.Printf("Failed to parse target time: %v", err)
			return
		}

		// Protection against duplicate webhooks
		if a.isDuplicateSnooze(e.Repo.Owner.GetLogin(), e.Repo.GetName(), e.Issue.GetNumber(), username, targetTime) {
			log.Printf("Ignoring duplicate snooze request")
			return
		}

		err = InsertSnooze(a.DB, e.Repo.Owner.GetLogin(), e.Repo.GetName(), e.Issue.GetNumber(), username, targetTime, installationID)
		if err != nil {
			log.Printf("Failed to insert snooze: %v", err)
		} else {
			log.Printf("Snooze inserted for %s at %v", username, targetTime)
		}
		return
	}

	loc := GetUserLocation(ctx, client, username)

	now := time.Now()
	targetTime, err := ParseTargetTime(cmd.DateString, loc, now)
	if err != nil {
		log.Printf("Failed to parse target time: %v", err)
		return
	}

	// Protection against duplicate webhooks
	if a.isDuplicateSnooze(e.Repo.Owner.GetLogin(), e.Repo.GetName(), e.Issue.GetNumber(), username, targetTime) {
		log.Printf("Ignoring duplicate snooze request")
		return
	}

	err = InsertSnooze(a.DB, e.Repo.Owner.GetLogin(), e.Repo.GetName(), e.Issue.GetNumber(), username, targetTime, installationID)
	if err != nil {
		log.Printf("Failed to insert snooze: %v", err)
	} else {
		log.Printf("Snooze inserted for %s at %v", username, targetTime)
	}
}

// isDuplicateSnooze checks if a very similar snooze already exists
func (a *App) isDuplicateSnooze(owner, repo string, issueID int, username string, targetTime time.Time) bool {
	query := `
	SELECT COUNT(*) FROM snoozes
	WHERE repo_owner = ? AND repo_name = ? AND issue_id = ? AND username = ? AND ABS(strftime('%s', target_time) - strftime('%s', ?)) < 60
	`
	var count int
	err := a.DB.QueryRow(query, owner, repo, issueID, username, targetTime.UTC().Format(time.RFC3339)).Scan(&count)
	if err != nil {
		log.Printf("Error checking for duplicate snooze: %v", err)
		return false
	}
	return count > 0
}

func RunDaemon() {
	cfg := loadConfig()

	// Initialize GitHub client
	client := github.NewClient(nil).WithAuthToken(cfg.GitHubToken)

	// Initialize Database
	db, err := InitDB(cfg.DatabaseFile)
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}
	defer func() {
		_ = db.Close()
	}()

	app := &App{
		Config: cfg,
		Client: client,
		DB:     db,
	}

	ctx := context.Background()
	app.StartBackgroundChecker(ctx, 1*time.Minute)

	http.HandleFunc("/webhook", app.handleWebhook)

	addr := fmt.Sprintf(":%s", cfg.Port)
	log.Printf("Server listening on %s", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
