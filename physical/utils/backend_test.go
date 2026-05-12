package physical_utils

import (
	"context"
	"errors"
	"testing"

	"github.com/openbao/openbao/sdk/v2/physical"
	"github.com/stretchr/testify/require"
)

func TestGenericSQLBackend_HAGating(t *testing.T) {
	t.Parallel()

	backend, err := NewGenericSQLBackend(GenericSQLBackendConfig{
		Client:  fakeExecutor{},
		Dialect: SQLiteDialect{},
		Table:   "test",
	})
	require.NoError(t, err)
	require.False(t, backend.HAEnabled())
	require.ErrorIs(t, backend.SetHAEnabled(true), ErrHANotSupported)
	require.False(t, backend.HAEnabled())
}

func TestGenericSQLBackend_GetNoRows(t *testing.T) {
	t.Parallel()

	backend, err := NewGenericSQLBackend(GenericSQLBackendConfig{
		Client:  fakeExecutor{row: fakeRow{err: ErrNoRows}},
		Dialect: SQLiteDialect{},
		Table:   "test",
	})
	require.NoError(t, err)

	entry, err := backend.Get(context.Background(), "missing")
	require.NoError(t, err)
	require.Nil(t, entry)
}

func TestGenericSQLBackend_CRUDUsesSplitKey(t *testing.T) {
	t.Parallel()

	exec := &recordingExecutor{row: fakeRow{value: []byte("value")}}
	backend, err := NewGenericSQLBackend(GenericSQLBackendConfig{
		Client:  exec,
		Dialect: SQLiteDialect{},
		Table:   "test",
	})
	require.NoError(t, err)

	err = backend.Put(context.Background(), &physical.Entry{Key: "foo/bar/baz", Value: []byte("value")})
	require.NoError(t, err)
	require.Equal(t, []any{"/foo/", "/foo/bar/", "baz", []byte("value")}, exec.execArgs)

	entry, err := backend.Get(context.Background(), "foo/bar/baz")
	require.NoError(t, err)
	require.Equal(t, []byte("value"), entry.Value)
	require.Equal(t, []any{"/foo/bar/", "baz"}, exec.queryOneArgs)
}

func TestDialects(t *testing.T) {
	t.Parallel()

	require.Equal(t, "$3", PostgresDialect{}.Placeholder(3))
	require.Equal(t, "?", SQLiteDialect{}.Placeholder(3))
	require.Equal(t, `"`+"table"+`"`, SQLiteDialect{}.QuoteIdent("table"))
	require.True(t, PostgresDialect{}.IsAlreadyExistsError(errors.New("SQLSTATE 23505")))
	require.True(t, PostgresDialect{}.IsReadOnlyError(errors.New("SQLSTATE 25006")))
}

type fakeExecutor struct {
	row Row
}

func (f fakeExecutor) ExecContext(context.Context, string, ...any) error { return nil }
func (f fakeExecutor) QueryOneContext(context.Context, string, ...any) (Row, error) {
	return f.row, nil
}
func (f fakeExecutor) QueryContext(context.Context, string, ...any) (Rows, error) {
	return fakeRows{}, nil
}

type recordingExecutor struct {
	row          Row
	execArgs     []any
	queryOneArgs []any
}

func (r *recordingExecutor) ExecContext(_ context.Context, _ string, args ...any) error {
	r.execArgs = args
	return nil
}
func (r *recordingExecutor) QueryOneContext(_ context.Context, _ string, args ...any) (Row, error) {
	r.queryOneArgs = args
	return r.row, nil
}
func (r *recordingExecutor) QueryContext(context.Context, string, ...any) (Rows, error) {
	return fakeRows{}, nil
}

type fakeRow struct {
	value []byte
	err   error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*(dest[0].(*[]byte)) = r.value
	return nil
}

type fakeRows struct{}

func (fakeRows) Next() bool        { return false }
func (fakeRows) Scan(...any) error { return nil }
func (fakeRows) Close() error      { return nil }
func (fakeRows) Err() error        { return nil }
