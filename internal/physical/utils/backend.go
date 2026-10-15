package physical_utils

import (
	"context"
	"errors"
	"fmt"
	"strings"

	log "github.com/hashicorp/go-hclog"
	"github.com/openbao/openbao/sdk/v2/physical"
)

var ErrHANotSupported = errors.New("ha not supported by this SQL backend")

type GenericSQLBackend struct {
	client  SQLExecutor
	dialect SQLDialect
	logger  log.Logger

	table           string
	tableConstraint string
	index           string

	putQuery             string
	getQuery             string
	deleteQuery          string
	listQuery            string
	listPageQuery        string
	listPageLimitedQuery string

	haEnabled   bool
	haSupported bool
}

type GenericSQLBackendConfig struct {
	Client          SQLExecutor
	Dialect         SQLDialect
	Logger          log.Logger
	Table           string
	TableConstraint string
	Index           string
	HAEnabled       bool
	HASupported     bool
}

func NewGenericSQLBackend(conf GenericSQLBackendConfig) (*GenericSQLBackend, error) {
	if conf.Client == nil {
		return nil, errors.New("missing SQL client")
	}
	if conf.Dialect == nil {
		return nil, errors.New("missing SQL dialect")
	}
	if conf.Table == "" {
		conf.Table = "openbao_kv_store"
	}
	if conf.Logger == nil {
		conf.Logger = log.NewNullLogger()
	}

	table := conf.Dialect.QuoteIdent(conf.Table)
	tableConstraint := conf.TableConstraint
	if tableConstraint == "" {
		tableConstraint = conf.Table + "_pkey"
	}
	index := conf.Index
	if index == "" {
		index = conf.Table + "_idx"
	}

	b := &GenericSQLBackend{
		client:          conf.Client,
		dialect:         conf.Dialect,
		logger:          conf.Logger,
		table:           table,
		tableConstraint: conf.Dialect.QuoteIdent(tableConstraint),
		index:           conf.Dialect.QuoteIdent(index),
		haSupported:     conf.HASupported,
	}
	if err := b.SetHAEnabled(conf.HAEnabled); err != nil {
		return nil, err
	}
	b.putQuery = conf.Dialect.PutQuery(table)
	b.getQuery = "SELECT value FROM " + table + " WHERE path = " + conf.Dialect.Placeholder(1) + " AND key = " + conf.Dialect.Placeholder(2)
	b.deleteQuery = "DELETE FROM " + table + " WHERE path = " + conf.Dialect.Placeholder(1) + " AND key = " + conf.Dialect.Placeholder(2)
	b.listQuery = conf.Dialect.ListQuery(table)
	b.listPageQuery = conf.Dialect.ListPageQuery(table, false)
	b.listPageLimitedQuery = conf.Dialect.ListPageQuery(table, true)

	return b, nil
}

func (b *GenericSQLBackend) CreateTables(ctx context.Context) error {
	collation := b.dialect.TextCollation()
	if collation != "" {
		collation = " " + collation
	}
	createTableQuery := "CREATE TABLE IF NOT EXISTS " + b.table + " (" +
		"parent_path TEXT" + collation + " NOT NULL," +
		" path TEXT" + collation + "," +
		" key TEXT" + collation + "," +
		" value " + b.dialect.BinaryType() + "," +
		" CONSTRAINT " + b.tableConstraint + " PRIMARY KEY (path, key)" +
		");"
	if err := b.client.ExecContext(ctx, createTableQuery); err != nil {
		if b.dialect.IsReadOnlyError(err) || b.dialect.IsAlreadyExistsError(err) {
			b.logger.Warn("Skipping table creation", "err", err)
			return nil
		}
		return fmt.Errorf("failed to execute create query: %w", err)
	}

	createIndexQuery := "CREATE INDEX IF NOT EXISTS " + b.index + " ON " + b.table + " (parent_path);"
	if err := b.client.ExecContext(ctx, createIndexQuery); err != nil {
		if b.dialect.IsAlreadyExistsError(err) {
			b.logger.Warn("Skipping index creation", "err", err)
			return nil
		}
		return fmt.Errorf("failed to create index on table: %w", err)
	}
	return nil
}

func (b *GenericSQLBackend) SetHAEnabled(enabled bool) error {
	if enabled && !b.haSupported {
		return ErrHANotSupported
	}
	b.haEnabled = enabled
	return nil
}

func (b *GenericSQLBackend) HAEnabled() bool {
	return b.haEnabled && b.haSupported
}

func (b *GenericSQLBackend) splitKey(fullPath string) (string, string, string) {
	var parentPath string
	var path string

	pieces := strings.Split(fullPath, "/")
	depth := len(pieces)
	key := pieces[depth-1]

	switch depth {
	case 1:
		parentPath = ""
		path = "/"
	case 2:
		parentPath = "/"
		path = "/" + pieces[0] + "/"
	default:
		parentPath = "/" + strings.Join(pieces[:depth-2], "/") + "/"
		path = "/" + strings.Join(pieces[:depth-1], "/") + "/"
	}

	return parentPath, path, key
}

func (b *GenericSQLBackend) Put(ctx context.Context, entry *physical.Entry) error {
	parentPath, path, key := b.splitKey(entry.Key)
	return b.client.ExecContext(ctx, b.putQuery, parentPath, path, key, entry.Value)
}

func (b *GenericSQLBackend) Get(ctx context.Context, fullPath string) (*physical.Entry, error) {
	_, path, key := b.splitKey(fullPath)

	var result []byte
	row, err := b.client.QueryOneContext(ctx, b.getQuery, path, key)
	if err != nil {
		return nil, err
	}
	err = row.Scan(&result)
	if b.dialect.IsNoRowsError(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	return &physical.Entry{Key: fullPath, Value: result}, nil
}

func (b *GenericSQLBackend) Delete(ctx context.Context, fullPath string) error {
	_, path, key := b.splitKey(fullPath)
	return b.client.ExecContext(ctx, b.deleteQuery, path, key)
}

func (b *GenericSQLBackend) List(ctx context.Context, prefix string) ([]string, error) {
	rows, err := b.client.QueryContext(ctx, b.listQuery, b.dialect.ListArgs("/"+prefix)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanKeys(rows)
}

func (b *GenericSQLBackend) ListPage(ctx context.Context, prefix string, after string, limit int) ([]string, error) {
	query := b.listPageQuery
	if limit > 0 {
		query = b.listPageLimitedQuery
	}
	rows, err := b.client.QueryContext(ctx, query, b.dialect.ListPageArgs("/"+prefix, after, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanKeys(rows)
}

func scanKeys(rows Rows) ([]string, error) {
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("failed to scan rows: %w", err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return keys, nil
}
