// Command ai-chat is the ContextGrip AI Chat API server: a self-hosted NL->SQL
// chat service over one PostgreSQL database, per openapi.yaml at the repo
// root.
package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/contextgrip-io/agent-sdk/server/internal/api"
	"github.com/contextgrip-io/agent-sdk/server/internal/approvalstore"
	"github.com/contextgrip-io/agent-sdk/server/internal/assistant"
	"github.com/contextgrip-io/agent-sdk/server/internal/chatstore"
	"github.com/contextgrip-io/agent-sdk/server/internal/dbx"
	"github.com/contextgrip-io/agent-sdk/server/internal/taskstore"
	"github.com/contextgrip-io/agent-sdk/server/internal/tokenstore"
	"github.com/contextgrip-io/agent-sdk/server/internal/trainingstore"
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	port := env("PORT", "8080")
	dbPath := env("AI_CHAT_DB_PATH", "./data/ai-chat.sqlite")
	modelID := env("AI_CHAT_MODEL", assistant.DefaultModel)
	baseURL := env("AI_CHAT_ANTHROPIC_BASE_URL", os.Getenv("ANTHROPIC_BASE_URL"))
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	databaseURL := os.Getenv("DATABASE_URL")
	writeDatabaseURL := os.Getenv("AI_CHAT_WRITE_DATABASE_URL")
	accessToken := os.Getenv("APP_ACCESS_TOKEN")
	devNoAuth := os.Getenv("AI_CHAT_DEV_NO_AUTH") == "1"
	features := api.ParseFeatures(os.Getenv("AI_CHAT_FEATURES"))
	agentMaxSteps := api.DefaultAgentMaxSteps
	if raw := os.Getenv("AI_CHAT_AGENT_MAX_STEPS"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			log.Fatalf("AI_CHAT_AGENT_MAX_STEPS must be a positive integer, got %q", raw)
		}
		agentMaxSteps = parsed
	}

	cfg := api.Config{ModelID: modelID, DevNoAuth: devNoAuth, Features: features, AgentMaxSteps: agentMaxSteps}
	log.Printf("features enabled: %s", strings.Join(features, ", "))

	// Auth is mandatory unless explicitly disabled for local development.
	switch {
	case accessToken != "":
		sum := sha256.Sum256([]byte(accessToken))
		cfg.PrimaryTokenSHA256 = sum[:]
		if devNoAuth {
			log.Printf("WARNING: AI_CHAT_DEV_NO_AUTH=1 ignored because APP_ACCESS_TOKEN is set")
			cfg.DevNoAuth = false
		}
	case devNoAuth:
		log.Printf("WARNING: running WITHOUT AUTHENTICATION (AI_CHAT_DEV_NO_AUTH=1). Every request is treated as the primary token. Local development only — never expose this instance.")
	default:
		log.Fatal("APP_ACCESS_TOKEN is required (generate one with `openssl rand -hex 32`). " +
			"Set AI_CHAT_DEV_NO_AUTH=1 to run without auth for local development only.")
	}

	// Conversation store and token store share one SQLite file (two table
	// sets), both opened with WAL + busy_timeout.
	chat, err := chatstore.New(dbPath)
	if err != nil {
		log.Fatalf("open chat store at %s: %v", dbPath, err)
	}
	defer chat.Close()
	tokens, err := tokenstore.New(dbPath)
	if err != nil {
		log.Fatalf("open token store at %s: %v", dbPath, err)
	}
	defer tokens.Close()
	training, err := trainingstore.New(dbPath)
	if err != nil {
		log.Fatalf("open training store at %s: %v", dbPath, err)
	}
	defer training.Close()
	approvals, err := approvalstore.New(dbPath)
	if err != nil {
		log.Fatalf("open approval store at %s: %v", dbPath, err)
	}
	defer approvals.Close()
	tasks, err := taskstore.New(dbPath)
	if err != nil {
		log.Fatalf("open task store at %s: %v", dbPath, err)
	}
	defer tasks.Close()
	cfg.Chat = chat
	cfg.Tokens = tokens
	cfg.Training = training
	cfg.Approvals = approvals
	cfg.Tasks = tasks

	// Chat endpoints serve 503 NOT_CONFIGURED until both of these are set;
	// UI and health endpoints work regardless.
	if apiKey != "" {
		cfg.Model = assistant.NewAnthropicClient(apiKey, modelID, baseURL)
	} else {
		log.Printf("ANTHROPIC_API_KEY is not set; chat endpoints will return 503 NOT_CONFIGURED")
	}
	if databaseURL != "" {
		db := dbx.Open(databaseURL)
		defer db.Close()
		cfg.DB = db
		// Non-secret identity for training-export lines (hash of
		// host:port/dbname + database name — never credentials).
		cfg.ConnectionID, cfg.ConnectionName = dbx.ConnectionIdentity(databaseURL)
	} else {
		log.Printf("DATABASE_URL is not set; chat endpoints will return 503 NOT_CONFIGURED")
	}
	if writeDatabaseURL != "" {
		writeDB := dbx.Open(writeDatabaseURL)
		defer writeDB.Close()
		cfg.WriteDB = writeDB
		log.Printf("write connection configured; approved writes will execute")
	} else {
		log.Printf("AI_CHAT_WRITE_DATABASE_URL is not set; approvals can only be rejected")
	}

	apiServer := api.New(cfg)
	server := &http.Server{
		Addr:              ":" + port,
		Handler:           apiServer,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Single background board runner; stops on the same shutdown signal.
	boardEnabled := false
	for _, f := range features {
		boardEnabled = boardEnabled || f == "board"
	}
	if boardEnabled {
		go apiServer.RunTaskWorker(ctx)
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("ai-chat %s listening on :%s (model %s)", api.Version, port, modelID)
		errCh <- server.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("serve: %v", err)
		}
	case <-ctx.Done():
		log.Printf("shutting down...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("graceful shutdown failed: %v", err)
			_ = server.Close()
		}
	}
}
