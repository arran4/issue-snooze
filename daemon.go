package snoozebot

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v62/github"
)

// Config holds the configuration for the bot
type Config struct {
	GitHubToken             string
	GitHubAppID             int64
	GitHubAppIDRaw          string // Tracks if it was supplied but invalid
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
			appID = -1 // Use -1 to represent an explicitly invalid ID
		}
	}

	cfg := Config{
		GitHubToken:             getEnvOrSecret("GITHUB_TOKEN_FILE", "GITHUB_TOKEN", ""),
		GitHubAppID:             appID,
		GitHubAppIDRaw:          appIDStr,
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

	// Cache for GitHub App transports to allow token renewal reuse.
	appsTransport   *ghinstallation.AppsTransport
	appTransports   map[int64]*ghinstallation.Transport
	appTransportMux sync.Mutex

	// Test seams and checker timings. Production leaves these unset.
	clientOverride     func(int64) (*github.Client, error)
	claimLeaseDuration time.Duration
	claimRenewInterval time.Duration
}

func isAppModeConfig(cfg Config) bool {
	return cfg.GitHubAppID > 0 && cfg.GitHubAppPrivateKeyFile != "" && cfg.WebhookSecret != ""
}

func isPATModeConfig(cfg Config) bool {
	return cfg.GitHubToken != ""
}

func validateConfig(cfg Config) error {
	if cfg.GitHubAppIDRaw != "" && cfg.GitHubAppID <= 0 {
		return fmt.Errorf("GITHUB_APP_ID must be a valid positive integer")
	}
	appFieldsPresent := cfg.GitHubAppID > 0 || cfg.GitHubAppPrivateKeyFile != "" || cfg.GitHubAppIDRaw != ""
	appMode := isAppModeConfig(cfg)
	patMode := isPATModeConfig(cfg)

	if appFieldsPresent && !appMode {
		return fmt.Errorf("partial GitHub App configuration detected. You must provide App ID, Private Key File, and Webhook Secret")
	}

	if appMode && patMode {
		log.Printf("Both GitHub App and PAT are configured. Proceeding in App Mode exclusively.")
		return nil
	}

	if !appMode && !patMode {
		return fmt.Errorf("no valid authentication method configured")
	}

	return nil
}

// getClient returns a GitHub client authenticated for the specific installation if possible.
// It explicitly enforces boundaries: App mode requests must use App credentials, and PAT is only for PAT mode.
func (a *App) getClient(installationID int64) (*github.Client, error) {
	if a.clientOverride != nil {
		return a.clientOverride(installationID)
	}

	if isAppModeConfig(a.Config) {
		if installationID <= 0 {
			return nil, fmt.Errorf("GitHub App mode requires a valid installation ID but none was provided")
		}

		a.appTransportMux.Lock()
		defer a.appTransportMux.Unlock()

		if a.appTransports == nil {
			a.appTransports = make(map[int64]*ghinstallation.Transport)
		}
		if a.appsTransport == nil {
			appsTransport, err := ghinstallation.NewAppsTransportKeyFromFile(http.DefaultTransport, a.Config.GitHubAppID, a.Config.GitHubAppPrivateKeyFile)
			if err != nil {
				return nil, fmt.Errorf("failed to create ghinstallation apps transport: %w", err)
			}
			a.appsTransport = appsTransport
		}

		itr, ok := a.appTransports[installationID]
		if !ok {
			itr = ghinstallation.NewFromAppsTransport(a.appsTransport, installationID)
			a.appTransports[installationID] = itr
		}

		return github.NewClient(&http.Client{Transport: itr}), nil
	}

	// PAT mode is isolated from App mode and is only used when App mode is not configured.
	if isPATModeConfig(a.Config) && a.Client != nil {
		return a.Client, nil
	}

	return nil, fmt.Errorf("no valid authentication method available")
}

func (a *App) evictInstallationTransport(installationID int64) {
	if installationID <= 0 {
		return
	}
	a.appTransportMux.Lock()
	defer a.appTransportMux.Unlock()
	if a.appTransports != nil {
		delete(a.appTransports, installationID)
	}
}

func (a *App) handleWebhook(w http.ResponseWriter, r *http.Request) {
	var payload []byte
	var err error

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

	var handlerErr error
	switch e := event.(type) {
	case *github.IssueCommentEvent:
		handlerErr = a.handleIssueComment(e, deliveryID)
	case *github.InstallationEvent:
		handlerErr = a.handleInstallationEvent(e)
	case *github.InstallationRepositoriesEvent:
		handlerErr = a.handleInstallationRepositoriesEvent(e)
	case *github.RepositoryEvent:
		handlerErr = a.handleRepositoryEvent(e)
	case *github.InstallationTargetEvent:
		handlerErr = a.handleInstallationTargetEvent(e)
	}

	if handlerErr != nil {
		if _, ok := handlerErr.(*ClientError); ok {
			log.Printf("Webhook request malformed: %v", handlerErr)
			http.Error(w, "Bad Request", http.StatusBadRequest)
		} else {
			log.Printf("Webhook handling failed (retryable): %v", handlerErr)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (a *App) handleInstallationEvent(e *github.InstallationEvent) error {
	if e == nil || e.Action == nil || e.Installation == nil || e.Installation.GetID() <= 0 {
		return &ClientError{Err: fmt.Errorf("installation event missing action or installation ID")}
	}
	installationID := e.Installation.GetID()
	switch *e.Action {
	case "deleted":
		a.evictInstallationTransport(installationID)
		log.Printf("Installation %d has been deleted. Cleaning up snoozes.", installationID)
		if err := DeleteSnoozesByInstallation(a.DB, installationID); err != nil {
			return fmt.Errorf("failed to delete snoozes for removed installation %d: %w", installationID, err)
		}
	case "suspend":
		a.evictInstallationTransport(installationID)
		log.Printf("Installation %d has been suspended. Snoozes remain queued for retry after authorization returns.", installationID)
	case "unsuspend", "new_permissions_accepted":
		a.evictInstallationTransport(installationID)
		log.Printf("Installation %d authorization changed (%s). Cached installation auth was cleared.", installationID, *e.Action)
	}
	return nil
}

func (a *App) handleInstallationRepositoriesEvent(e *github.InstallationRepositoriesEvent) error {
	if e.Action != nil && *e.Action == "removed" {
		for _, repo := range e.RepositoriesRemoved {
			log.Printf("Repository %s removed from installation %d. Cleaning up snoozes.", repo.GetFullName(), e.Installation.GetID())
			if err := DeleteSnoozesByRepoAndInstallation(a.DB, repo.GetFullName(), e.Installation.GetID()); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *App) handleRepositoryEvent(e *github.RepositoryEvent) error {
	if e == nil || e.Action == nil || (*e.Action != "renamed" && *e.Action != "transferred") {
		return nil
	}
	if e.Repo == nil || e.Repo.Owner == nil || e.Installation == nil || e.Installation.GetID() <= 0 || e.Changes == nil {
		return fmt.Errorf("repository %s event missing repository, installation, or changes data", *e.Action)
	}
	newOwner := e.Repo.Owner.GetLogin()
	newName := e.Repo.GetName()
	if newOwner == "" || newName == "" {
		return fmt.Errorf("repository %s event missing new repository identity", *e.Action)
	}
	oldOwner := newOwner
	oldName := newName
	switch *e.Action {
	case "renamed":
		if e.Changes.Repo == nil || e.Changes.Repo.Name == nil || e.Changes.Repo.Name.From == nil || *e.Changes.Repo.Name.From == "" {
			return fmt.Errorf("repository renamed event missing old repository name")
		}
		oldName = *e.Changes.Repo.Name.From
	case "transferred":
		if e.Changes.Owner == nil || e.Changes.Owner.OwnerInfo == nil {
			return fmt.Errorf("repository transferred event missing old owner")
		}
		if user := e.Changes.Owner.OwnerInfo.User; user != nil {
			oldOwner = user.GetLogin()
		} else if org := e.Changes.Owner.OwnerInfo.Org; org != nil {
			oldOwner = org.GetLogin()
		}
		if oldOwner == "" {
			return fmt.Errorf("repository transferred event missing old owner login")
		}
		if e.Changes.Repo != nil && e.Changes.Repo.Name != nil && e.Changes.Repo.Name.From != nil && *e.Changes.Repo.Name.From != "" {
			oldName = *e.Changes.Repo.Name.From
		}
	}
	if err := UpdateSnoozesRepoIdentity(a.DB, oldOwner, oldName, newOwner, newName, e.Installation.GetID()); err != nil {
		return fmt.Errorf("failed to reconcile repository %s: %w", *e.Action, err)
	}
	return nil
}

func (a *App) handleInstallationTargetEvent(e *github.InstallationTargetEvent) error {
	if e == nil || e.Action == nil || *e.Action != "renamed" {
		return nil
	}
	if e.Installation == nil || e.Installation.GetID() <= 0 || e.Account == nil || e.Account.GetLogin() == "" || e.Changes == nil || e.Changes.Login == nil || e.Changes.Login.From == nil || *e.Changes.Login.From == "" {
		return fmt.Errorf("installation target renamed event missing old/new login or installation ID")
	}
	if err := UpdateSnoozesInstallationOwner(a.DB, *e.Changes.Login.From, e.Account.GetLogin(), e.Installation.GetID()); err != nil {
		return fmt.Errorf("failed to reconcile installation target rename: %w", err)
	}
	return nil
}

type ClientError struct{ Err error }

func (e *ClientError) Error() string { return e.Err.Error() }

func (a *App) handleIssueComment(e *github.IssueCommentEvent, deliveryID string) error {
	if e.Action == nil || *e.Action != "created" {
		return nil
	}
	if e.Comment.User != nil && e.Comment.User.GetType() == "Bot" {
		return nil
	}
	cmd := ParseCommand(e.Comment.GetBody(), a.Config.BotCommand)
	if !cmd.HasCommand {
		return nil
	}
	if deliveryID == "" {
		return &ClientError{Err: fmt.Errorf("missing X-GitHub-Delivery header")}
	}
	var installationID int64
	if e.Installation != nil {
		installationID = e.Installation.GetID()
	}
	client, err := a.getClient(installationID)
	if err != nil {
		return err
	}
	loc, locErr := GetUserLocation(context.Background(), client, e.Sender.GetLogin())
	if locErr != nil {
		var statusErr *GitHubStatusError
		if errors.As(locErr, &statusErr) && (statusErr.StatusCode == http.StatusUnauthorized || statusErr.StatusCode == http.StatusForbidden) {
			a.evictInstallationTransport(installationID)
		}
		return locErr
	}
	targetTime, err := ParseTargetTime(cmd.DateString, loc, time.Now())
	if err != nil {
		return &ClientError{Err: fmt.Errorf("failed to parse target time: %v", err)}
	}
	err = a.insertSnoozeIdempotent(deliveryID, e.Repo.Owner.GetLogin(), e.Repo.GetName(), e.Issue.GetNumber(), e.Sender.GetLogin(), targetTime, installationID)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return nil
		}
		return err
	}
	return nil
}

func (a *App) insertSnoozeIdempotent(deliveryID, owner, repo string, issueID int, username string, targetTime time.Time, installationID int64) error {
	tx, err := a.DB.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.Exec(`INSERT INTO processed_deliveries (delivery_id, processed_at) VALUES (?, ?)`, deliveryID, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return fmt.Errorf("delivery marker failed: %w", err)
	}
	if _, err = tx.Exec(`INSERT INTO snoozes (repo_owner, repo_name, issue_id, username, target_time, installation_id) VALUES (?, ?, ?, ?, ?, ?)`, owner, repo, issueID, username, targetTime.UTC().Format(time.RFC3339), installationID); err != nil {
		return fmt.Errorf("failed to insert snooze inside transaction: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	go func() {
		_, _ = a.DB.Exec(`DELETE FROM processed_deliveries WHERE processed_at < ?`, time.Now().Add(-7*24*time.Hour).UTC().Format(time.RFC3339))
	}()
	return nil
}

func RunDaemon() {
	cfg := loadConfig()
	if err := validateConfig(cfg); err != nil {
		log.Fatalf("Invalid configuration: %v", err)
	}
	var client *github.Client
	if isPATModeConfig(cfg) && !isAppModeConfig(cfg) {
		client = github.NewClient(nil).WithAuthToken(cfg.GitHubToken)
	}
	db, err := InitDB(cfg.DatabaseFile)
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}
	defer func() { _ = db.Close() }()
	app := &App{Config: cfg, Client: client, DB: db}
	ctx := context.Background()
	app.StartBackgroundChecker(ctx, time.Minute)
	http.HandleFunc("/webhook", app.handleWebhook)
	addr := fmt.Sprintf(":%s", cfg.Port)
	log.Printf("Server listening on %s", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
