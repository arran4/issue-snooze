package snoozebot

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/go-github/v62/github"
)

// Config holds the configuration for the bot
type Config struct {
	GitHubToken   string
	WebhookSecret string
	BotCommand    string
	Port          string
	DatabaseFile  string
}

// loadConfig loads configuration from environment variables or Docker secrets
func loadConfig() Config {
	cfg := Config{
		GitHubToken:   getEnvOrSecret("GITHUB_TOKEN_FILE", "GITHUB_TOKEN", ""),
		WebhookSecret: getEnvOrSecret("WEBHOOK_SECRET_FILE", "WEBHOOK_SECRET", ""),
		BotCommand:    getEnv("BOT_COMMAND", "@snooze"),
		Port:          getEnv("PORT", "8080"),
		DatabaseFile:  getEnv("DATABASE_FILE", "snooze.db"),
	}
	return cfg
}

func getEnv(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return fallback
}

func getEnvOrSecret(fileKey, envKey, fallback string) string {
	if filePath, exists := os.LookupEnv(fileKey); exists {
		content, err := os.ReadFile(filePath)
		if err == nil {
			return strings.TrimSpace(string(content))
		}
		log.Printf("Failed to read secret file %s: %v", filePath, err)
	}
	if value, exists := os.LookupEnv(envKey); exists {
		return value
	}
	return fallback
}

// App holds the application state
type App struct {
	Config Config
	Client *github.Client
	DB     *sql.DB
}

func (a *App) handleWebhook(w http.ResponseWriter, r *http.Request) {
	var payload []byte
	var err error

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

	ctx := context.Background()
	username := e.Sender.GetLogin()
	loc := GetUserLocation(ctx, a.Client, username)

	now := time.Now()
	targetTime, err := ParseTargetTime(cmd.DateString, loc, now)
	if err != nil {
		log.Printf("Failed to parse target time: %v", err)
		return
	}

	err = InsertSnooze(a.DB, e.Repo.Owner.GetLogin(), e.Repo.GetName(), e.Issue.GetNumber(), username, targetTime)
	if err != nil {
		log.Printf("Failed to insert snooze: %v", err)
	} else {
		log.Printf("Snooze inserted for %s at %v", username, targetTime)
	}
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
	defer db.Close()

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
