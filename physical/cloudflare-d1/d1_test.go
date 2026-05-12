package cloudflare_d1

import (
	"testing"

	"github.com/cloudflare/cloudflare-go/v6/d1"
	physical_utils "github.com/openbao/openbao/physical/utils"
	"github.com/stretchr/testify/require"
)

func TestRequireStaticSealEnv(t *testing.T) {
	t.Setenv("BAO_STATIC_SEAL_KEY", "")

	err := requireStaticSealEnv(map[string]string{})
	require.Error(t, err)

	err = requireStaticSealEnv(map[string]string{"require_static_seal_env": "false"})
	require.NoError(t, err)

	t.Setenv("BAO_STATIC_SEAL_KEY", "01234567890123456789012345678901")
	err = requireStaticSealEnv(map[string]string{})
	require.NoError(t, err)
}

func TestD1RowsScan(t *testing.T) {
	t.Parallel()

	results := []d1.QueryResult{
		{
			Success: true,
			Results: []interface{}{
				map[string]interface{}{"key": "foo"},
				map[string]interface{}{"key": "bar"},
			},
		},
	}

	rows := NewD1Rows(&results)
	require.True(t, rows.Next())
	var first string
	require.NoError(t, rows.Scan(&first))
	require.Equal(t, "foo", first)

	require.True(t, rows.Next())
	var second string
	require.NoError(t, rows.Scan(&second))
	require.Equal(t, "bar", second)

	require.False(t, rows.Next())
	require.NoError(t, rows.Err())
}

func TestD1RowNoRows(t *testing.T) {
	t.Parallel()

	results := []d1.QueryResult{{Success: true}}
	row := &D1Row{rows: NewD1Rows(&results)}

	var value []byte
	err := row.Scan(&value)
	require.ErrorIs(t, err, physical_utils.ErrNoRows)
}
