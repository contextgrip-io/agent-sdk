package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
)

type authRole int

const (
	roleNone authRole = iota
	roleNamed
	rolePrimary
)

type authRoleKey struct{}

func roleFrom(ctx context.Context) authRole {
	if v, ok := ctx.Value(authRoleKey{}).(authRole); ok {
		return v
	}
	return roleNone
}

// authMiddleware guards /api/v1/*. The primary APP_ACCESS_TOKEN is compared
// via SHA-256 + constant-time compare; named tokens are looked up by hash in
// the token store (lastUsedAt updated best-effort).
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.DevNoAuth {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), authRoleKey{}, rolePrimary)))
			return
		}
		raw := bearerToken(r)
		if raw == "" {
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing bearer token")
			return
		}
		sum := sha256.Sum256([]byte(raw))
		if len(s.cfg.PrimaryTokenSHA256) == sha256.Size &&
			subtle.ConstantTimeCompare(sum[:], s.cfg.PrimaryTokenSHA256) == 1 {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), authRoleKey{}, rolePrimary)))
			return
		}
		if s.cfg.Tokens != nil {
			token, err := s.cfg.Tokens.FindByHash(r.Context(), hex.EncodeToString(sum[:]))
			if err == nil && token != nil {
				// Best-effort usage stamp; a failure must not fail the request.
				_ = s.cfg.Tokens.TouchLastUsed(r.Context(), token.ID)
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), authRoleKey{}, roleNamed)))
				return
			}
		}
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid bearer token")
	})
}

// requireAdmin restricts token management to the primary APP_ACCESS_TOKEN.
func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if roleFrom(r.Context()) != rolePrimary {
			writeError(w, http.StatusForbidden, "ADMIN_REQUIRED", "token management requires the primary access token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	if header == "" {
		return ""
	}
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}
