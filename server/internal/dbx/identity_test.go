package dbx

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConnectionIdentity(t *testing.T) {
	t.Parallel()

	id, name := ConnectionIdentity("postgres://user:s3cret@db.example.com:5433/mydb?sslmode=disable")
	require.Equal(t, "mydb", name)
	sum := sha256.Sum256([]byte("db.example.com:5433/mydb"))
	require.Equal(t, hex.EncodeToString(sum[:])[:12], id)
	require.Len(t, id, 12)
	// The identity is derived only from host:port/dbname — never credentials.
	require.NotContains(t, id, "s3cret")
	require.NotContains(t, name, "s3cret")

	// Default port applies when the URL omits it.
	idDefault, _ := ConnectionIdentity("postgres://user:pw@db.example.com/mydb")
	sumDefault := sha256.Sum256([]byte("db.example.com:5432/mydb"))
	require.Equal(t, hex.EncodeToString(sumDefault[:])[:12], idDefault)

	// Same endpoint, different credentials -> same identity.
	id2, _ := ConnectionIdentity("postgres://other:different@db.example.com:5433/mydb")
	require.Equal(t, id, id2)

	// key=value DSN form parses too.
	idDSN, nameDSN := ConnectionIdentity("host=db.example.com port=5433 dbname=mydb user=u password=p")
	require.Equal(t, id, idDSN)
	require.Equal(t, "mydb", nameDSN)

	// Unparseable input yields a fixed placeholder, not an error or a leak.
	idBad, nameBad := ConnectionIdentity("::::not-a-url::::")
	require.Len(t, idBad, 12)
	require.Empty(t, nameBad)
}
