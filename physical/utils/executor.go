package physical_utils

import (
	"context"
	"database/sql"
)

type SQLExecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) error
	QueryOneContext(ctx context.Context, query string, args ...any) (Row, error)
	QueryContext(ctx context.Context, query string, args ...any) (Rows, error)
}

type Row interface {
	Scan(dest ...any) error
}

type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Close() error
	Err() error
}

type SQLDB struct {
	DB *sql.DB
}

func (s SQLDB) ExecContext(ctx context.Context, query string, args ...any) error {
	_, err := s.DB.ExecContext(ctx, query, args...)
	return err
}

func (s SQLDB) QueryOneContext(ctx context.Context, query string, args ...any) (Row, error) {
	return s.DB.QueryRowContext(ctx, query, args...), nil
}

func (s SQLDB) QueryContext(ctx context.Context, query string, args ...any) (Rows, error) {
	return s.DB.QueryContext(ctx, query, args...)
}
