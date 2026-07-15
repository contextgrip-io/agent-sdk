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
	"syscall"
	"time"

	"github.com/contextgrip-io/agent-sdk/server/internal/api"
	"github.com/contextgrip-io/agent-sdk/server/internal/assistant"
	"github.com/contextgrip-io/agent-sdk/server/internal/chatstore"
	"github.com/contextgrip-io/agent-sdk/server/internal/dbx"
	"github.com/contextgrip-io/agent-sdk/server/internal/tokenstore"
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
	accessToken := os.Getenv("APP_ACCESS_TOKEN")
	devNoAuth := os.Getenv("AI_CHAT_DEV_NO_AUTH") == "1"

	cfg := api.Config{ModelID: modelID, DevNoAuth: devNoAuth}

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
	cfg.Chat = chat
	cfg.Tokens = tokens

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
	} else {
		log.Printf("DATABASE_URL is not set; chat endpoints will return 503 NOT_CONFIGURED")
	}

	server := &http.Server{
		Addr:              ":" + port,
		Handler:           api.New(cfg),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
