package api

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAuthMatrix walks the full 401/403 matrix including the named-token
// mint -> use -> revoke lifecycle.
func TestAuthMatrix(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)

	// No token / malformed / wrong token -> 401 UNAUTHORIZED.
	for name, header := range map[string]string{
		"missing": "",
		"wrong":   "not-the-token",
	} {
		rec := env.do(t, http.MethodGet, "/api/v1/status", header, nil)
		require.Equal(t, http.StatusUnauthorized, rec.Code, name)
		require.Equal(t, "UNAUTHORIZED", decodeBody[errorBody](t, rec).Code, name)
	}

	// Primary token works everywhere.
	require.Equal(t, http.StatusOK, env.do(t, http.MethodGet, "/api/v1/status", testPrimaryToken, nil).Code)

	// Mint a named token (admin-only).
	rec := env.do(t, http.MethodPost, "/api/v1/tokens", testPrimaryToken, map[string]string{"label": "reporting-cron"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	created := decodeBody[struct {
		ID          string `json:"id"`
		Label       string `json:"label"`
		Fingerprint string `json:"fingerprint"`
		Token       string `json:"token"`
		CreatedAt   string `json:"createdAt"`
	}](t, rec)
	require.NotEmpty(t, created.ID)
	require.Equal(t, "reporting-cron", created.Label)
	require.Len(t, created.Fingerprint, 8)
	require.Len(t, created.Token, 64)
	require.NotEmpty(t, created.CreatedAt)

	// The named token authenticates ordinary endpoints...
	require.Equal(t, http.StatusOK, env.do(t, http.MethodGet, "/api/v1/status", created.Token, nil).Code)
	require.Equal(t, http.StatusOK, env.do(t, http.MethodGet, "/api/v1/conversations", created.Token, nil).Code)

	// ...but not token management: 403 ADMIN_REQUIRED.
	for _, probe := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/api/v1/tokens", nil},
		{http.MethodPost, "/api/v1/tokens", map[string]string{"label": "x"}},
		{http.MethodDelete, "/api/v1/tokens/" + created.ID, nil},
	} {
		rec := env.do(t, probe.method, probe.path, created.Token, probe.body)
		require.Equal(t, http.StatusForbidden, rec.Code, probe.path)
		require.Equal(t, "ADMIN_REQUIRED", decodeBody[errorBody](t, rec).Code, probe.path)
	}

	// Using the named token stamped lastUsedAt (best-effort, but visible here).
	listRec := env.do(t, http.MethodGet, "/api/v1/tokens", testPrimaryToken, nil)
	list := decodeBody[[]tokenView](t, listRec)
	require.Len(t, list, 1)
	require.NotEmpty(t, list[0].LastUsedAt)
	// The raw token value is never returned by the list endpoint.
	require.NotContains(t, listRec.Body.String(), created.Token)

	// Revoke with the primary token; the named token stops working.
	rec = env.do(t, http.MethodDelete, "/api/v1/tokens/"+created.ID, testPrimaryToken, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, decodeBody[map[string]bool](t, rec)["deleted"])

	rec = env.do(t, http.MethodGet, "/api/v1/status", created.Token, nil)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Equal(t, "UNAUTHORIZED", decodeBody[errorBody](t, rec).Code)
}

func TestCreateTokenValidation(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)

	rec := env.do(t, http.MethodPost, "/api/v1/tokens", testPrimaryToken, map[string]string{"label": "  "})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "VALIDATION", decodeBody[errorBody](t, rec).Code)

	long := make([]byte, 121)
	for i := range long {
		long[i] = 'a'
	}
	rec = env.do(t, http.MethodPost, "/api/v1/tokens", testPrimaryToken, map[string]string{"label": string(long)})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "VALIDATION", decodeBody[errorBody](t, rec).Code)
}

// TestUnauthenticatedSurfaces: health, metrics, and the static UI never
// require a token.
func TestUnauthenticatedSurfaces(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)

	rec := env.do(t, http.MethodGet, "/healthz", "", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "ok", rec.Body.String())

	require.Equal(t, http.StatusOK, env.do(t, http.MethodGet, "/metrics", "", nil).Code)

	// Static UI at / and SPA fallback for unknown non-API GETs.
	rec = env.do(t, http.MethodGet, "/", "", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "ContextGrip AI Chat")
	rec = env.do(t, http.MethodGet, "/conversations/some-client-route", "", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "ContextGrip AI Chat")

	// Unknown API routes are JSON errors, not SPA fallbacks: 401 without a
	// token (auth guards the whole /api/v1 tree), 404 with one.
	rec = env.do(t, http.MethodGet, "/api/v1/does-not-exist", "", nil)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Equal(t, "UNAUTHORIZED", decodeBody[errorBody](t, rec).Code)
	rec = env.do(t, http.MethodGet, "/api/v1/does-not-exist", testPrimaryToken, nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Equal(t, "NOT_FOUND", decodeBody[errorBody](t, rec).Code)
	// Unknown routes under /api but outside /api/v1 are unauthenticated 404s.
	rec = env.do(t, http.MethodGet, "/api/other", "", nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Equal(t, "NOT_FOUND", decodeBody[errorBody](t, rec).Code)
}

// TestDevNoAuth: AI_CHAT_DEV_NO_AUTH=1 treats every request as the primary
// token (local development only).
func TestDevNoAuth(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, func(cfg *Config) {
		cfg.PrimaryTokenSHA256 = nil
		cfg.DevNoAuth = true
	})
	require.Equal(t, http.StatusOK, env.do(t, http.MethodGet, "/api/v1/status", "", nil).Code)
	require.Equal(t, http.StatusOK, env.do(t, http.MethodGet, "/api/v1/tokens", "", nil).Code)
}
