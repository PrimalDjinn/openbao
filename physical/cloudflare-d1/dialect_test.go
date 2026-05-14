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
	require.Contains(t, query, "WHERE parent_path = ?")
}

func TestD1Dialect_ListArgs(t *testing.T) {
	dialect := D1Dialect{}

	// Test without trailing slash
	args := dialect.ListArgs("/foo")
	require.Len(t, args, 5)
	for i := 0; i < 5; i++ {
		require.Equal(t, "/foo/", args[i], "Arg %d should have trailing slash", i)
	}

	// Test with trailing slash
	args = dialect.ListArgs("/bar/")
	require.Len(t, args, 5)
	for i := 0; i < 5; i++ {
		require.Equal(t, "/bar/", args[i], "Arg %d should keep trailing slash", i)
	}
}

func TestD1Dialect_ListPageQuery(t *testing.T) {
	dialect := D1Dialect{}
	table := dialect.QuoteIdent("my_table")
	query := dialect.ListPageQuery(table, true)
	require.Contains(t, query, "LIMIT ?")
	require.Contains(t, query, "SELECT key FROM \"my_table\" WHERE path = ? AND key > ?")
	require.Contains(t, query, "WHERE parent_path = ?")
}

func TestD1Dialect_ListPageArgs(t *testing.T) {
	dialect := D1Dialect{}

	args := dialect.ListPageArgs("/foo", "after", 10)
	require.Len(t, args, 10) // 9 + 1 (limit)
	require.Equal(t, "/foo/", args[0])
	require.Equal(t, "after", args[1])
	require.Equal(t, "/foo/", args[2])
	require.Equal(t, "/foo/", args[3])
	require.Equal(t, "/foo/", args[4])
	require.Equal(t, "/foo/", args[5])
	require.Equal(t, "/foo/", args[6])
	require.Equal(t, "/foo/", args[7])
	require.Equal(t, "after", args[8])
	require.Equal(t, 10, args[9])
}
