package dbx

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// ConnectionIdentity derives a stable, non-secret identity for the configured
// database, used to tag training-export lines: name is the database name and
// id is the first 12 hex chars of SHA-256("host:port/dbname"). Credentials
// are never part of the identity. Both URL and key=value DSN forms are
// accepted; an unparseable value yields a fixed placeholder identity rather
// than an error (the export must never leak why parsing failed, because the
// raw value may embed credentials).
func ConnectionIdentity(databaseURL string) (id, name string) {
	host, port, dbname := "unknown", "0", "unknown"
	if cfg, err := pgconn.ParseConfig(databaseURL); err == nil {
		if cfg.Host != "" {
			host = cfg.Host
		}
		if cfg.Port != 0 {
			port = fmt.Sprintf("%d", cfg.Port)
		}
		if cfg.Database != "" {
			dbname = cfg.Database
			name = cfg.Database
		}
	}
	sum := sha256.Sum256([]byte(host + ":" + port + "/" + dbname))
	return hex.EncodeToString(sum[:])[:12], name
}
