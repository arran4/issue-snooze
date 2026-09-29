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

	// Cache for GitHub App transports to allow token renewal reuse
	appTransports   map[int64]*ghinstallation.Transport
	appTransportMux sync.Mutex
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
	if isAppModeConfig(a.Config) {
		if installationID <= 0 {
			return nil, fmt.Errorf("GitHub App mode requires a valid installation ID but none was provided")
		}

		a.appTransportMux.Lock()
		defer a.appTransportMux.Unlock()

		if a.appTransports == nil {
			a.appTransports = make(map[int64]*ghinstallation.Transport)
		}

		itr, ok := a.appTransports[installationID]
		if !ok {
			// To fulfill requirements 3, 4, 8:
			// "To properly fulfill installation-token caching and renewal, we should cache this transport."
			var err error
			appsTransport, err := ghinstallation.NewAppsTransportKeyFromFile(http.DefaultTransport, a.Config.GitHubAppID, a.Config.GitHubAppPrivateKeyFile)
			if err != nil {
				return nil, fmt.Errorf("failed to create ghinstallation apps transport: %w", err)
			}
			itr = ghinstallation.NewFromAppsTransport(appsTransport, installationID)
			a.appTransports[installationID] = itr
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
	default:
		// Not an event we process, ignore
	}

	if handlerErr != nil {
		if _, ok := handlerErr.(*ClientError); ok {
			log.Printf("Webhook request malformed: %v", handlerErr)
			http.Error(w, "Bad Request", http.StatusBadRequest)
		} else {
			log.Printf("Webhook handling failed (retryable): %v", handlerErr)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	} else {
		w.WriteHeader(http.StatusOK)
	}
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

func (a *App) handleInstallationEvent(e *github.InstallationEvent) error {
	if e.Action == nil || e.Installation == nil {
		return nil
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
		log.Printf("Installation %d has been suspended. Snoozes are preserved and will retry after authorization returns.", installationID)
	case "unsuspend", "new_permissions_accepted":
		a.evictInstallationTransport(installationID)
		log.Printf("Installation %d authorization changed; cached installation auth was evicted.", installationID)
	}
	return nil
}

func (a *App) handleInstallationRepositoriesEvent(e *github.InstallationRepositoriesEvent) error {
	if e.Action == nil || *e.Action != "removed" || e.Installation == nil {
		return nil
	}
	for _, repo := range e.RepositoriesRemoved {
		log.Printf("Repository %s removed from installation %d. Cleaning up snoozes.", repo.GetFullName(), e.Installation.GetID())
		if err := DeleteSnoozesByRepoAndInstallation(a.DB, repo.GetFullName(), e.Installation.GetID()); err != nil {
			return fmt.Errorf("failed to delete snoozes for removed repo %s on installation %d: %w", repo.GetFullName(), e.Installation.GetID(), err)
		}
	}
	return nil
}

func (a *App) handleRepositoryEvent(e *github.RepositoryEvent) error {
	if e.Action == nil || (*e.Action != "renamed" && *e.Action != "transferred") {
		return nil
	}
	if e.Repo == nil || e.Installation == nil || e.Changes == nil {
		return fmt.Errorf("repository %s missing repository, installation, or changes payload", *e.Action)
	}

	newOwner := e.Repo.Owner.GetLogin()
	newName := e.Repo.GetName()
	oldOwner := newOwner
	oldName := newName

	if e.Changes.Repo != nil && e.Changes.Repo.Name != nil && e.Changes.Repo.Name.From != nil {
		oldName = *e.Changes.Repo.Name.From
	}

	switch *e.Action {
	case "renamed":
		if oldName == "" || oldName == newName {
			return fmt.Errorf("repository renamed missing old repository name")
		}
	case "transferred":
		if e.Changes.Owner == nil || e.Changes.Owner.OwnerInfo == nil {
			return fmt.Errorf("repository transferred missing old owner identity")
		}
		info := e.Changes.Owner.OwnerInfo
		if info.User != nil {
			oldOwner = info.User.GetLogin()
		} else if info.Org != nil {
			oldOwner = info.Org.GetLogin()
		}
		if oldOwner == "" || oldOwner == newOwner {
			return fmt.Errorf("repository transferred missing old owner identity")
		}
	}

	log.Printf("Repository %s was %s. Updating mapped snoozes from %s/%s to %s/%s", e.Repo.GetFullName(), *e.Action, oldOwner, oldName, newOwner, newName)
	if err := UpdateSnoozesRepoIdentity(a.DB, oldOwner, oldName, newOwner, newName, e.Installation.GetID()); err != nil {
		return fmt.Errorf("failed to update snoozes for %s repo %s/%s to %s/%s: %w", *e.Action, oldOwner, oldName, newOwner, newName, err)
	}
	return nil
}

func (a *App) handleInstallationTargetEvent(e *github.InstallationTargetEvent) error {
	if e.Action == nil || *e.Action != "renamed" {
		return nil
	}
	if e.Installation == nil || e.Account == nil || e.Changes == nil || e.Changes.Login == nil || e.Changes.Login.From == nil {
		return fmt.Errorf("installation target rename missing old or new account identity")
	}
	oldOwner := *e.Changes.Login.From
	newOwner := e.Account.GetLogin()
	if oldOwner == "" || newOwner == "" || oldOwner == newOwner {
		return fmt.Errorf("installation target rename missing old or new account identity")
	}
	log.Printf("Installation target %s renamed from %s. Updating existing snoozes.", newOwner, oldOwner)
	return UpdateSnoozesInstallationOwner(a.DB, oldOwner, newOwner, e.Installation.GetID())
}

type ClientError struct {
	Err error
}

func (e *ClientError) Error() string {
	return e.Err.Error()
}

func (a *App) handleIssueComment(e *github.IssueCommentEvent, deliveryID string) error {
	if e.Action == nil || *e.Action != "created" {
		return nil
	}

	// Issue 6: Ignore bot-authored comments to avoid self/automation loops
	if e.Comment.User != nil && e.Comment.User.GetType() == "Bot" {
		log.Printf("Ignoring bot-authored comment")
		return nil
	}

	body := e.Comment.GetBody()
	cmd := ParseCommand(body, a.Config.BotCommand)
	if !cmd.HasCommand {
		return nil
	}

	log.Printf("Received snooze command on repo %s for issue %d", e.Repo.GetFullName(), e.Issue.GetNumber())

	if deliveryID == "" {
		// Delivery ID is strongly required for actual webhooks to preserve idempotency guarantees.
		log.Printf("Rejecting webhook without delivery ID to preserve idempotency guarantees.")
		return &ClientError{Err: fmt.Errorf("missing X-GitHub-Delivery header")}
	}

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
		// We return a 500 error here to prompt GitHub to retry if it's transient
		return err
	}

	loc, locErr := GetUserLocation(ctx, client, username)
	if locErr != nil {
		log.Printf("Transient error resolving user location for timezone: %v", locErr)
		return locErr
	}

	now := time.Now()
	targetTime, err := ParseTargetTime(cmd.DateString, loc, now)
	if err != nil {
		log.Printf("Failed to parse target time: %v", err)
		// Return client error since this is bad input
		return &ClientError{Err: fmt.Errorf("failed to parse target time: %v", err)}
	}

	// Webhook delivery idempotency with a transaction tied to successful handling.
	err = a.insertSnoozeIdempotent(deliveryID, e.Repo.Owner.GetLogin(), e.Repo.GetName(), e.Issue.GetNumber(), username, targetTime, installationID)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			log.Printf("Ignoring duplicate webhook delivery %s", deliveryID)
			return nil
		} else {
			log.Printf("Failed to insert snooze idempotently: %v", err)
			return err
		}
	} else {
		log.Printf("Snooze inserted for %s at %v", username, targetTime)
	}

	return nil
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

	if err := tx.Commit(); err != nil {
		return err
	}

	// Clean up old deliveries (e.g. older than 7 days) after successful commit
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
