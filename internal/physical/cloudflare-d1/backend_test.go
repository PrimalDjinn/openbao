package cloudflare_d1

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/openbao/openbao/sdk/v2/physical"
	physical_utils "github.com/openbao/openbao/v2/internal/physical/utils"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func newTestBackend(t *testing.T) (*physical_utils.GenericSQLBackend, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "d1.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	backend, err := physical_utils.NewGenericSQLBackend(physical_utils.GenericSQLBackendConfig{
		Client: physical_utils.SQLDB{DB: db}, Dialect: D1Dialect{},
	})
	require.NoError(t, err)
	require.NoError(t, backend.CreateTables(context.Background()))
	return backend, db
}

func TestD1Backend(t *testing.T) {
	t.Parallel()
	backend, _ := newTestBackend(t)
	physical.ExerciseBackend(t, backend)
	physical.ExerciseBackend_ListPrefix(t, backend)
}

func assertListings(t *testing.T, backend physical.Backend, prefix string, expected []string) {
	t.Helper()
	ctx := context.Background()
	keys, err := backend.List(ctx, prefix)
	require.NoError(t, err)
	require.Equal(t, expected, keys, "List(%q)", prefix)
	for _, limit := range []int{-1, 0, len(expected) + 1} {
		page, err := backend.ListPage(ctx, prefix, "", limit)
		require.NoError(t, err)
		require.Equal(t, expected, page, "ListPage(%q, limit=%d)", prefix, limit)
	}
	var paged []string
	after := ""
	for range len(expected) + 1 {
		page, err := backend.ListPage(ctx, prefix, after, 1)
		require.NoError(t, err)
		if len(page) == 0 {
			break
		}
		require.Len(t, page, 1)
		paged = append(paged, page[0])
		after = page[0]
	}
	require.Equal(t, expected, paged, "paginated ListPage(%q)", prefix)
}

func TestD1Backend_AncestorsWithoutMarkers(t *testing.T) {
	t.Parallel()
	backend, _ := newTestBackend(t)
	ctx := context.Background()
	entries := []string{
		"proj/staging/private/app/deployment",
		"proj/staging/private/app/terminals/tenant-a",
		"proj/staging/private/app/terminals/tenant-b",
		"proj/staging/private/ops/config",
		"proj/production/private/app/deployment",
	}
	for _, key := range entries {
		require.NoError(t, backend.Put(ctx, &physical.Entry{Key: key, Value: []byte(key)}))
	}
	assertListings(t, backend, "", []string{"proj/"})
	assertListings(t, backend, "proj/", []string{"production/", "staging/"})
	assertListings(t, backend, "proj/staging/", []string{"private/"})
	assertListings(t, backend, "proj/staging/private/", []string{"app/", "ops/"})
	assertListings(t, backend, "proj/staging/private/app/", []string{"deployment", "terminals/"})

	// A document and the equally named prefix can coexist. Removing the
	// document must not remove the prefix while descendants still exist.
	markers := []string{"proj", "proj/staging", "proj/staging/private", "proj/staging/private/app"}
	for _, key := range markers {
		require.NoError(t, backend.Put(ctx, &physical.Entry{Key: key, Value: []byte("marker")}))
	}
	assertListings(t, backend, "", []string{"proj", "proj/"})
	assertListings(t, backend, "proj/staging/", []string{"private", "private/"})
	assertListings(t, backend, "proj/staging/private/", []string{"app", "app/", "ops/"})
	for _, key := range markers {
		require.NoError(t, backend.Delete(ctx, key))
		entry, err := backend.Get(ctx, key)
		require.NoError(t, err)
		require.Nil(t, entry)
	}
	assertListings(t, backend, "", []string{"proj/"})
	assertListings(t, backend, "proj/", []string{"production/", "staging/"})
	assertListings(t, backend, "proj/staging", []string{"private/"})
	assertListings(t, backend, "proj/staging/private", []string{"app/", "ops/"})
	for _, key := range entries {
		entry, err := backend.Get(ctx, key)
		require.NoError(t, err)
		require.NotNil(t, entry)
		require.Equal(t, []byte(key), entry.Value)
	}
	for _, key := range entries {
		require.NoError(t, backend.Delete(ctx, key))
	}
	assertListings(t, backend, "", nil)
	assertListings(t, backend, "proj/staging/private/", nil)
}

func TestD1Backend_LiteralPrefixBoundaries(t *testing.T) {
	t.Parallel()
	backend, _ := newTestBackend(t)
	ctx := context.Background()
	prefixes := []string{"private", "private-old", "private0", "PRIVATE", "p%", "p_", "pX", "p[a]", "pa", "秘密", "秘密別"}
	for _, prefix := range prefixes {
		key := prefix + "/" + prefix + "/nested/value"
		require.NoError(t, backend.Put(ctx, &physical.Entry{Key: key, Value: []byte("value")}))
	}
	for _, prefix := range prefixes {
		t.Run(prefix, func(t *testing.T) {
			for _, suffix := range []string{"", "/"} {
				assertListings(t, backend, prefix+suffix, []string{prefix + "/"})
			}
		})
	}
	assertListings(t, backend, "missing/", nil)
}

func TestD1Backend_DescendantRangeUsesIndex(t *testing.T) {
	t.Parallel()
	_, db := newTestBackend(t)
	dialect := D1Dialect{}
	for _, query := range []struct {
		sql  string
		args []any
	}{
		{dialect.ListQuery("openbao_kv_store"), dialect.ListArgs("/proj/")},
		{dialect.ListPageQuery("openbao_kv_store", true), dialect.ListPageArgs("/proj/", "", 10)},
	} {
		rows, err := db.Query("EXPLAIN QUERY PLAN "+query.sql, query.args...)
		require.NoError(t, err)
		var plan string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
			plan += detail + "\n"
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		require.Contains(t, plan, "USING INDEX openbao_kv_store_idx (parent_path>? AND parent_path<?)")
		require.NotContains(t, plan, "SCAN openbao_kv_store")
	}
}
