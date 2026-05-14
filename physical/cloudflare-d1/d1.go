package cloudflare_d1

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/cloudflare/cloudflare-go/v6"
	"github.com/cloudflare/cloudflare-go/v6/d1"
	"github.com/cloudflare/cloudflare-go/v6/option"
	log "github.com/hashicorp/go-hclog"
	"github.com/openbao/openbao/api/v2"
	physical_utils "github.com/openbao/openbao/physical/utils"
	"github.com/openbao/openbao/sdk/v2/physical"
)

// Verify cloudflare_d1 satisfies the correct interfaces
var (
	_ physical.Backend = (*CloudflareD1Backend)(nil)
	// TODO: Skipped for now
	// _ physical.TransactionalBackend = (*D1SQLBackend)(nil)
)

type D1Executor struct {
	db *DB
}

func (d *D1Executor) ExecContext(ctx context.Context, query string, args ...any) error {
	results, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	for _, result := range *results {
		if !result.Success {
			return errors.New("cloudflare d1 query failed")
		}
	}
	return err
}

type DB struct {
	db    *d1.DatabaseService
	creds D1ConnectionCredentials
}

// CloudflareD1Backend is a physical backend that stores data
// within a Cloudflare D1 database.
type CloudflareD1Backend struct {
	*physical_utils.GenericSQLBackend

	client *DB
}

type D1ConnectionCredentials struct {
	AccountID  string
	APIKey     string
	DatabaseID string
}

func NewCloudflareD1Backend(conf map[string]string, logger log.Logger) (physical.Backend, error) {
	if err := requireStaticSealEnv(conf); err != nil {
		return nil, err
	}
	if conf["ha_enabled"] == "true" {
		return nil, physical_utils.ErrHANotSupported
	}

	creds, err := connectionURL(conf)
	if err != nil {
		return nil, err
	}
	client := cloudflare.NewClient(option.WithAPIToken(creds.APIKey))
	db := &DB{
		db:    client.D1.Database,
		creds: *creds,
	}
	if ok, err := db.Ping(); err != nil || !ok {
		return nil, fmt.Errorf("failed to connect to Cloudflare D1: %w", err)
	}

	table := conf["table"]
	if table == "" {
		table = "openbao_kv_store"
	}
	generic, err := physical_utils.NewGenericSQLBackend(physical_utils.GenericSQLBackendConfig{
		Client:      &D1Executor{db: db},
		Dialect:     D1Dialect{},
		Logger:      logger,
		Table:       table,
		HASupported: false,
	})
	if err != nil {
		return nil, err
	}
	if conf["skip_create_table"] != "true" {
		if err := generic.CreateTables(context.Background()); err != nil {
			return nil, err
		}
	}
	return &CloudflareD1Backend{
		GenericSQLBackend: generic,
		client:            db,
	}, nil
}

func requireStaticSealEnv(conf map[string]string) error {
	if conf["require_static_seal_env"] == "false" {
		return nil
	}
	if api.ReadBaoVariable("BAO_STATIC_SEAL_KEY") == "" && os.Getenv("BAO_STATIC_SEAL_KEY") == "" {
		return errors.New("cloudflare D1 storage requires BAO_STATIC_SEAL_KEY to be set for static seal configuration")
	}
	return nil
}

// connectionURL first checks the environment variables for a connection URL. If
// no connection URL exists in the environment variable, the Vault config file is
// checked. If neither the environment variables or the config file set the
// connection URL for the D1 backend, because it is a required field, an error is returned.
func connectionURL(conf map[string]string) (*D1ConnectionCredentials, error) {
	connURL := conf["connection_url"]
	if envURL := api.ReadBaoVariable("BAO_D1_CONNECTION_URL"); envURL != "" {
		connURL = envURL
	}

	if connURL == "" {
		accountID := api.ReadBaoVariable("BAO_D1_ACCOUNT_ID")
		databaseID := api.ReadBaoVariable("BAO_D1_DATABASE_ID")
		apiToken := api.ReadBaoVariable("BAO_D1_TOKEN")
		if apiToken == "" {
			apiToken = api.ReadBaoVariable("BAO_D1_API_TOKEN")
		}

		if accountID != "" && databaseID != "" && apiToken != "" {
			return &D1ConnectionCredentials{
				AccountID:  accountID,
				APIKey:     apiToken,
				DatabaseID: databaseID,
			}, nil
		}
		return nil, errors.New("connection_url is required (or set BAO_D1_ACCOUNT_ID, BAO_D1_DATABASE_ID, and BAO_D1_TOKEN)")
	}

	u, err := url.Parse(connURL)
	if err != nil {
		return nil, err
	}

	apiKey, _ := u.User.Password()
	return &D1ConnectionCredentials{
		AccountID:  u.User.Username(),
		APIKey:     apiKey,
		DatabaseID: u.Host,
	}, nil
}

func (client *DB) Ping() (bool, error) {
	if client.db == nil {
		return false, errors.New("backend client is nil")
	}

	ctx := context.Background()
	_, err := client.db.Query(ctx, client.creds.DatabaseID, d1.DatabaseQueryParams{
		AccountID: cloudflare.F(client.creds.AccountID),
		Body: d1.DatabaseQueryParamsBody{
			Sql: cloudflare.F("SELECT 1"),
		},
	})
	return err == nil, err
}

func (client *DB) QueryContext(ctx context.Context, query string, args ...any) (*[]d1.QueryResult, error) {
	if client.db == nil {
		return nil, errors.New("backend client is nil")
	}

	// Cloudflare D1 API (via cloudflare-go) doesn't support raw []byte in JSON params.
	// We must encode binary data to base64 strings.
	encodedArgs := make([]interface{}, len(args))
	for i, arg := range args {
		if b, ok := arg.([]byte); ok {
			encodedArgs[i] = base64.StdEncoding.EncodeToString(b)
		} else {
			encodedArgs[i] = arg
		}
	}

	res, err := client.db.Query(ctx, client.creds.DatabaseID, d1.DatabaseQueryParams{
		AccountID: cloudflare.F(client.creds.AccountID),
		Body: d1.DatabaseQueryParamsBody{
			Sql:    cloudflare.F(query),
			Params: cloudflare.F[interface{}](encodedArgs),
		},
	})
	if err != nil {
		return nil, err
	}

	return &res.Result, nil
}

func (client *DB) Query(query string, args ...any) (*[]d1.QueryResult, error) {
	if client.db == nil {
		return nil, errors.New("backend client is nil")
	}

	ctx := context.Background()
	return client.QueryContext(ctx, query, args...)
}

func (d *D1Executor) QueryContext(ctx context.Context, query string, args ...any) (physical_utils.Rows, error) {
	results, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}

	return NewD1Rows(results), nil
}

func (d *D1Executor) QueryOneContext(ctx context.Context, query string, args ...any) (physical_utils.Row, error) {
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}

	return &D1Row{rows: rows}, nil
}

type D1Dialect struct {
	physical_utils.SQLiteDialect
}

func (D1Dialect) Name() string { return "cloudflare-d1" }

func (D1Dialect) ListQuery(table string) string {
	segment := "substr(path, length(?)+1)"
	child := "substr(" + segment + ", 1, instr(" + segment + ", '/'))"
	return "SELECT key FROM " + table + " WHERE path = ?" +
		" UNION ALL SELECT DISTINCT " + child + " FROM " + table +
		" WHERE parent_path = ? AND instr(" + segment + ", '/') > 0" +
		" ORDER BY key"
}

func (D1Dialect) ListArgs(path string) []any {
	p := path
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return []any{p, p, p, p, p}
}

func (D1Dialect) ListPageQuery(table string, limited bool) string {
	segment := "substr(path, length(?)+1)"
	child := "substr(" + segment + ", 1, instr(" + segment + ", '/'))"
	query := "SELECT key FROM " + table + " WHERE path = ? AND key > ?" +
		" UNION ALL SELECT DISTINCT " + child + " FROM " + table +
		" WHERE parent_path = ? AND instr(" + segment + ", '/') > 0 AND " + child + " > ?" +
		" ORDER BY key"
	if limited {
		query += " LIMIT ?"
	}
	return query
}

func (D1Dialect) ListPageArgs(path, after string, limit int) []any {
	p := path
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	args := []any{p, after, p, p, p, p, p, p, after}
	if limit > 0 {
		args = append(args, limit)
	}
	return args
}
