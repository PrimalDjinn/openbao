package physical_utils

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

var ErrNoRows = errors.New("sql backend: no rows")

type SQLDialect interface {
	Name() string
	Placeholder(n int) string
	BinaryType() string
	TextCollation() string
	TimestampType() string
	NowExpr() string
	QuoteIdent(name string) string
	IsNoRowsError(error) bool
	IsReadOnlyError(error) bool
	IsAlreadyExistsError(error) bool
	PutQuery(table string) string
	ListQuery(table string) string
	ListArgs(path string) []any
	ListPageQuery(table string, limited bool) string
	ListPageArgs(path, after string, limit int) []any
}

type PostgresDialect struct{}

func (PostgresDialect) Name() string             { return "postgresql" }
func (PostgresDialect) Placeholder(n int) string { return fmt.Sprintf("$%d", n) }
func (PostgresDialect) BinaryType() string       { return "BYTEA" }
func (PostgresDialect) TextCollation() string    { return `COLLATE "C"` }
func (PostgresDialect) TimestampType() string    { return "TIMESTAMP WITH TIME ZONE" }
func (PostgresDialect) NowExpr() string          { return "NOW()" }
func (PostgresDialect) QuoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
func (PostgresDialect) IsNoRowsError(err error) bool {
	return errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrNoRows)
}
func (PostgresDialect) IsReadOnlyError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "SQLSTATE 25006")
}
func (PostgresDialect) IsAlreadyExistsError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "SQLSTATE 23505")
}
func (d PostgresDialect) PutQuery(table string) string {
	return "INSERT INTO " + table + " VALUES($1, $2, $3, $4)" +
		" ON CONFLICT (path, key) DO " +
		" UPDATE SET (parent_path, path, key, value) = ($1, $2, $3, $4)"
}
func (PostgresDialect) ListQuery(table string) string {
	return "SELECT key FROM " + table + " WHERE path = $1" +
		" UNION ALL SELECT DISTINCT substring(substr(path, length($1)+1) from '^.*?/') FROM " + table +
		" WHERE parent_path LIKE $1 || '%'" +
		" ORDER BY key"
}
func (PostgresDialect) ListArgs(path string) []any {
	return []any{path}
}
func (PostgresDialect) ListPageQuery(table string, limited bool) string {
	query := "SELECT key FROM " + table + " WHERE path = $1 AND key > $2" +
		" UNION ALL SELECT DISTINCT substring(substr(path, length($1)+1) from '^.*?/') FROM " + table +
		" WHERE parent_path LIKE $1 || '%' AND substring(substr(path, length($1)+1) from '^.*?/') > $2" +
		" ORDER BY key"
	if limited {
		query += " LIMIT $3"
	}
	return query
}
func (PostgresDialect) ListPageArgs(path, after string, limit int) []any {
	if limit <= 0 {
		return []any{path, after}
	}
	return []any{path, after, limit}
}

type SQLiteDialect struct{}

func (SQLiteDialect) Name() string           { return "sqlite" }
func (SQLiteDialect) Placeholder(int) string { return "?" }
func (SQLiteDialect) BinaryType() string     { return "BLOB" }
func (SQLiteDialect) TextCollation() string  { return "" }
func (SQLiteDialect) TimestampType() string  { return "TIMESTAMP" }
func (SQLiteDialect) NowExpr() string        { return "CURRENT_TIMESTAMP" }
func (SQLiteDialect) QuoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
func (SQLiteDialect) IsNoRowsError(err error) bool {
	return errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrNoRows)
}
func (SQLiteDialect) IsReadOnlyError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "readonly") || strings.Contains(msg, "read-only")
}
func (SQLiteDialect) IsAlreadyExistsError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already exists") || strings.Contains(msg, "constraint failed")
}
func (SQLiteDialect) PutQuery(table string) string {
	return "INSERT INTO " + table + " (parent_path, path, key, value) VALUES(?, ?, ?, ?)" +
		" ON CONFLICT(path, key) DO UPDATE SET parent_path = excluded.parent_path, value = excluded.value"
}
func (SQLiteDialect) ListQuery(table string) string {
	segment := "substr(path, length(?)+1)"
	child := "substr(" + segment + ", 1, instr(" + segment + ", '/'))"
	return "SELECT key FROM " + table + " WHERE path = ?" +
		" UNION ALL SELECT DISTINCT " + child + " FROM " + table +
		" WHERE parent_path LIKE ? || '%' AND instr(" + segment + ", '/') > 0" +
		" ORDER BY key"
}
func (SQLiteDialect) ListArgs(path string) []any {
	return []any{path, path, path, path, path}
}
func (SQLiteDialect) ListPageQuery(table string, limited bool) string {
	segment := "substr(path, length(?)+1)"
	child := "substr(" + segment + ", 1, instr(" + segment + ", '/'))"
	query := "SELECT key FROM " + table + " WHERE path = ? AND key > ?" +
		" UNION ALL SELECT DISTINCT " + child + " FROM " + table +
		" WHERE parent_path LIKE ? || '%' AND instr(" + segment + ", '/') > 0 AND " + child + " > ?" +
		" ORDER BY key"
	if limited {
		query += " LIMIT ?"
	}
	return query
}
func (SQLiteDialect) ListPageArgs(path, after string, limit int) []any {
	args := []any{path, after, path, path, path, path, path, path, after}
	if limit > 0 {
		args = append(args, limit)
	}
	return args
}
