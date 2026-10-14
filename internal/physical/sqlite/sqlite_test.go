package sqlite

import (
	"path/filepath"
	"testing"

	log "github.com/hashicorp/go-hclog"
	"github.com/openbao/openbao/sdk/v2/physical"
	"github.com/stretchr/testify/require"
)

func TestSQLiteBackend(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "openbao.db")
	backend, err := NewSQLiteBackend(map[string]string{
		"path": path,
	}, log.NewNullLogger())
	require.NoError(t, err)
	t.Cleanup(func() {
		if closer, ok := backend.(*SQLiteBackend); ok {
			require.NoError(t, closer.Close())
		}
	})

	physical.ExerciseBackend(t, backend)
	physical.ExerciseBackend_ListPrefix(t, backend)
}

func TestSQLiteBackendRejectsHA(t *testing.T) {
	t.Parallel()

	_, err := NewSQLiteBackend(map[string]string{
		"path":       filepath.Join(t.TempDir(), "openbao.db"),
		"ha_enabled": "true",
	}, log.NewNullLogger())
	require.Error(t, err)
}
