package cloudflare_d1

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestD1Dialect_ListQuery(t *testing.T) {
	dialect := D1Dialect{}
	table := dialect.QuoteIdent("my_table")
	query := dialect.ListQuery(table)
	require.Contains(t, query, "SELECT key FROM \"my_table\" WHERE path = ?")
	require.Contains(t, query, "WHERE parent_path >= ? AND parent_path < ?")
}

func TestD1Dialect_ListArgs(t *testing.T) {
	dialect := D1Dialect{}

	for _, path := range []string{"/foo", "/foo/"} {
		require.Equal(t, []any{"/foo/", "/foo/", "/foo/", "/foo/", "/foo0", "/foo/"}, dialect.ListArgs(path))
	}
	require.Equal(t, []any{"/", "/", "/", "/", "0", "/"}, dialect.ListArgs("/"))
}

func TestD1Dialect_ListPageQuery(t *testing.T) {
	dialect := D1Dialect{}
	table := dialect.QuoteIdent("my_table")
	query := dialect.ListPageQuery(table, true)
	require.Contains(t, query, "LIMIT ?")
	require.Contains(t, query, "SELECT key FROM \"my_table\" WHERE path = ? AND key > ?")
	require.Contains(t, query, "WHERE parent_path >= ? AND parent_path < ?")
}

func TestD1Dialect_ListPageArgs(t *testing.T) {
	dialect := D1Dialect{}

	for _, path := range []string{"/foo", "/foo/"} {
		require.Equal(t, []any{"/foo/", "after", "/foo/", "/foo/", "/foo/", "/foo0", "/foo/", "/foo/", "/foo/", "after", 10}, dialect.ListPageArgs(path, "after", 10))
		require.Equal(t, []any{"/foo/", "after", "/foo/", "/foo/", "/foo/", "/foo0", "/foo/", "/foo/", "/foo/", "after"}, dialect.ListPageArgs(path, "after", 0))
	}
}
