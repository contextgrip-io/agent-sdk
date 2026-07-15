package api

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadyz(t *testing.T) {
	t.Parallel()

	t.Run("ready", func(t *testing.T) {
		t.Parallel()
		env := newTestEnv(t, nil)
		rec := env.do(t, http.MethodGet, "/readyz", "", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		body := decodeBody[readiness](t, rec)
		require.True(t, body.Ready)
		require.True(t, body.Model)
		require.True(t, body.Database)
	})

	t.Run("no model key", func(t *testing.T) {
		t.Parallel()
		env := newTestEnv(t, func(cfg *Config) { cfg.Model = nil })
		rec := env.do(t, http.MethodGet, "/readyz", "", nil)
		require.Equal(t, http.StatusServiceUnavailable, rec.Code)
		body := decodeBody[readiness](t, rec)
		require.False(t, body.Ready)
		require.False(t, body.Model)
		require.True(t, body.Database)
	})

	t.Run("db unreachable", func(t *testing.T) {
		t.Parallel()
		env := newTestEnv(t, nil)
		env.db.pingErr = errors.New("connection refused")
		rec := env.do(t, http.MethodGet, "/readyz", "", nil)
		require.Equal(t, http.StatusServiceUnavailable, rec.Code)
		body := decodeBody[readiness](t, rec)
		require.False(t, body.Ready)
		require.True(t, body.Model)
		require.False(t, body.Database)
	})
}

func TestStatus(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)
	rec := env.do(t, http.MethodGet, "/api/v1/status", testPrimaryToken, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	body := decodeBody[statusResponse](t, rec)
	require.Equal(t, Version, body.Version)
	require.Equal(t, "0.1.0", body.Version)
	require.Equal(t, "fake-model", body.Model)
	require.Equal(t, "postgresql", body.Engine)
	require.True(t, body.Ready)
}

func TestMetricsEndpoint(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)

	rec := env.do(t, http.MethodPost, "/api/v1/ask", testPrimaryToken, map[string]string{"question": "How many?"})
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, http.StatusOK, env.do(t, http.MethodGet, "/healthz", "", nil).Code)

	body := env.do(t, http.MethodGet, "/metrics", "", nil).Body.String()
	require.Contains(t, body, "# TYPE ai_chat_requests_total counter")
	require.Contains(t, body, `ai_chat_requests_total{class="chat"} 1`)
	require.Contains(t, body, `ai_chat_requests_total{class="system"}`)
	require.Contains(t, body, "# TYPE ai_chat_questions_total counter")
	require.Contains(t, body, "ai_chat_questions_total 1")
	require.Contains(t, body, "# TYPE ai_chat_model_errors_total counter")
	require.Contains(t, body, "ai_chat_model_errors_total 0")
}
