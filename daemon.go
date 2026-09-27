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

func isAppModeConfig(cfg Config) bool {
	return cfg.GitHubAppID > 0 && cfg.GitHubAppPrivateKeyFile != "" && cfg.WebhookSecret != ""
}

func isPATModeConfig(cfg Config) bool {
	return cfg.GitHubToken != ""
}

func validateConfig(cfg Config) error {
	appMode := isAppModeConfig(cfg)
	patMode := isPATModeConfig(cfg)

	if !appMode && !patMode {
		// Are there partial App settings?
		if cfg.GitHubAppID > 0 || cfg.GitHubAppPrivateKeyFile != "" || cfg.WebhookSecret != "" {
			return fmt.Errorf("partial GitHub App configuration detected. You must provide App ID, Private Key File, and Webhook Secret")
		}
		return fmt.Errorf("no valid authentication method configured")
	}

	if appMode && patMode {
		log.Printf("Both GitHub App and PAT are configured. Proceeding in App Mode exclusively.")
	}

	return nil
}

// getClient returns a GitHub client authenticated for the specific installation if possible.
// It explicitly enforces boundaries: App mode requests must use App credentials, and PAT is only for PAT mode.
func (a *App) getClient(installationID int64) (*github.Client, error) {
	if isAppModeConfig(a.Config) {
		if installationID <= 0 {
			return nil, fmt.Errorf("GitHub App mode requires a valid installation ID but none was provided")
		}
		itr, err := ghinstallation.NewKeyFromFile(http.DefaultTransport, a.Config.GitHubAppID, installationID, a.Config.GitHubAppPrivateKeyFile)
		if err != nil {
			return nil, fmt.Errorf("failed to create ghinstallation transport: %w", err)
		}
		return github.NewClient(&http.Client{Transport: itr}), nil
	}

	// Fallback to PAT mode ONLY if we are explicitly not in App Mode
	if isPATModeConfig(a.Config) && a.Client != nil {
		return a.Client, nil
	}

	return nil, fmt.Errorf("no valid authentication method available")
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

	deliveryID := github.DeliveryID(r)

	event, err := github.ParseWebHook(github.WebHookType(r), payload)
	if err != nil {
		log.Printf("Error parsing webhook: %v", err)
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	switch e := event.(type) {
	case *github.IssueCommentEvent:
		a.handleIssueComment(e, deliveryID)
	case *github.InstallationEvent:
		a.handleInstallationEvent(e)
	case *github.InstallationRepositoriesEvent:
		a.handleInstallationRepositoriesEvent(e)
	default:
		// Not an event we process, ignore
	}
}

func (a *App) handleInstallationEvent(e *github.InstallationEvent) {
	// Reconcile App lifecycle
	if e.Action != nil && (*e.Action == "deleted" || *e.Action == "suspend") {
		log.Printf("Installation %d has been %s. Cleaning up snoozes.", e.Installation.GetID(), *e.Action)
		err := DeleteSnoozesByInstallation(a.DB, e.Installation.GetID())
		if err != nil {
			log.Printf("Failed to delete snoozes for removed installation %d: %v", e.Installation.GetID(), err)
		}
	}
}

func (a *App) handleInstallationRepositoriesEvent(e *github.InstallationRepositoriesEvent) {
	if e.Action != nil && *e.Action == "removed" {
		for _, repo := range e.RepositoriesRemoved {
			log.Printf("Repository %s removed from installation %d. Cleaning up snoozes.", repo.GetFullName(), e.Installation.GetID())
			err := DeleteSnoozesByRepo(a.DB, repo.GetFullName())
			if err != nil {
				log.Printf("Failed to delete snoozes for removed repo %s: %v", repo.GetFullName(), err)
			}
		}
	}
}

func (a *App) handleIssueComment(e *github.IssueCommentEvent, deliveryID string) {
	if e.Action != nil && (*e.Action == "deleted" || *e.Action == "edited") {
		return
	}

	// Protection against duplicate webhooks
	if deliveryID != "" {
		if a.isDuplicateDelivery(deliveryID) {
			log.Printf("Ignoring duplicate webhook delivery %s", deliveryID)
			return
		}
		a.markDeliveryProcessed(deliveryID)
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

	// App-mode auth failure rejects command without persistence.
	client, err := a.getClient(installationID)
	if err != nil {
		log.Printf("Failed to establish authorization for App mode processing: %v. Command rejected.", err)
		return
	}

	loc := GetUserLocation(ctx, client, username)

	now := time.Now()
	targetTime, err := ParseTargetTime(cmd.DateString, loc, now)
	if err != nil {
		log.Printf("Failed to parse target time: %v", err)
		return
	}

	// Webhook delivery idempotency with a transaction tied to successful handling.
	if deliveryID != "" {
		err = a.insertSnoozeIdempotent(deliveryID, e.Repo.Owner.GetLogin(), e.Repo.GetName(), e.Issue.GetNumber(), username, targetTime, installationID)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE constraint failed") {
				log.Printf("Ignoring duplicate webhook delivery %s", deliveryID)
			} else {
				log.Printf("Failed to insert snooze idempotently: %v", err)
			}
		} else {
			log.Printf("Snooze inserted for %s at %v", username, targetTime)
		}
	} else {
		// Fallback for direct invocations without webhooks (e.g., tests)
		err = InsertSnooze(a.DB, e.Repo.Owner.GetLogin(), e.Repo.GetName(), e.Issue.GetNumber(), username, targetTime, installationID)
		if err != nil {
			log.Printf("Failed to insert snooze: %v", err)
		} else {
			log.Printf("Snooze inserted for %s at %v", username, targetTime)
		}
	}
}

func (a *App) insertSnoozeIdempotent(deliveryID, owner, repo string, issueID int, username string, targetTime time.Time, installationID int64) error {
	tx, err := a.DB.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	// Try inserting the delivery marker first. If this fails, it means we already processed this delivery.
	_, err = tx.Exec(`INSERT INTO processed_deliveries (delivery_id, processed_at) VALUES (?, ?)`, deliveryID, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("delivery marker failed: %w", err)
	}

	query := `
	INSERT INTO snoozes (repo_owner, repo_name, issue_id, username, target_time, installation_id)
	VALUES (?, ?, ?, ?, ?, ?)
	`
	_, err = tx.Exec(query, owner, repo, issueID, username, targetTime.UTC().Format(time.RFC3339), installationID)
	if err != nil {
		return fmt.Errorf("failed to insert snooze inside transaction: %w", err)
	}

	// Clean up old deliveries (e.g. older than 7 days) on a background thread so we don't slow down the request
	go func() {
		_, _ = a.DB.Exec(`DELETE FROM processed_deliveries WHERE processed_at < ?`, time.Now().Add(-7*24*time.Hour).UTC().Format(time.RFC3339))
	}()

	return tx.Commit()
}

// isDuplicateDelivery checks if this specific delivery has been seen
func (a *App) isDuplicateDelivery(deliveryID string) bool {
	if deliveryID == "" {
		return false
	}
	query := `SELECT COUNT(*) FROM processed_deliveries WHERE delivery_id = ?`
	var count int
	err := a.DB.QueryRow(query, deliveryID).Scan(&count)
	if err != nil {
		log.Printf("Error checking for duplicate delivery: %v", err)
		return false
	}
	return count > 0
}

func (a *App) markDeliveryProcessed(deliveryID string) {
	if deliveryID == "" {
		return
	}
	_, err := a.DB.Exec(`INSERT INTO processed_deliveries (delivery_id, processed_at) VALUES (?, ?)`, deliveryID, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		log.Printf("Failed to record processed delivery: %v", err)
	}

	// Clean up old deliveries (e.g. older than 7 days)
	_, _ = a.DB.Exec(`DELETE FROM processed_deliveries WHERE processed_at < ?`, time.Now().Add(-7*24*time.Hour).UTC().Format(time.RFC3339))
}

func RunDaemon() {
	cfg := loadConfig()

	if err := validateConfig(cfg); err != nil {
		log.Fatalf("Invalid configuration: %v", err)
	}

	// Initialize GitHub client
	var client *github.Client
	if isPATModeConfig(cfg) && !isAppModeConfig(cfg) {
		client = github.NewClient(nil).WithAuthToken(cfg.GitHubToken)
	}

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
