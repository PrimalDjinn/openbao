package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	log "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-secure-stdlib/parseutil"
	"github.com/openbao/openbao/sdk/v2/physical"
	physical_utils "github.com/openbao/openbao/v2/internal/physical/utils"
	_ "modernc.org/sqlite"
)

var _ physical.Backend = (*SQLiteBackend)(nil)

type SQLiteBackend struct {
	*physical_utils.GenericSQLBackend

	client *sql.DB
}

func NewSQLiteBackend(conf map[string]string, logger log.Logger) (physical.Backend, error) {
	path := conf["path"]
	if path == "" {
		path = conf["database"]
	}
	if path == "" {
		return nil, errors.New("sqlite storage requires path")
	}

	maxParInt := physical.DefaultParallelOperations
	if maxParStr := conf["max_parallel"]; maxParStr != "" {
		parsed, err := strconv.Atoi(maxParStr)
		if err != nil {
			return nil, fmt.Errorf("failed parsing max_parallel parameter: %w", err)
		}
		maxParInt = parsed
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(maxParInt)
	if maxParInt > 1 {
		db.SetMaxIdleConns(maxParInt)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}

	table := conf["table"]
	if table == "" {
		table = "openbao_kv_store"
	}
	generic, err := physical_utils.NewGenericSQLBackend(physical_utils.GenericSQLBackendConfig{
		Client:      physical_utils.SQLDB{DB: db},
		Dialect:     physical_utils.SQLiteDialect{},
		Logger:      logger,
		Table:       table,
		HASupported: false,
		HAEnabled:   conf["ha_enabled"] == "true",
	})
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	rawSkipCreateTable := conf["skip_create_table"]
	if rawSkipCreateTable == "" {
		rawSkipCreateTable = "false"
	}
	skipCreateTable, err := parseutil.ParseBool(rawSkipCreateTable)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to parse value for `skip_create_table`: %w", err)
	}
	if !skipCreateTable {
		if err := generic.CreateTables(context.Background()); err != nil {
			_ = db.Close()
			return nil, err
		}
	}

	return &SQLiteBackend{
		GenericSQLBackend: generic,
		client:            db,
	}, nil
}

func (b *SQLiteBackend) Close() error {
	return b.client.Close()
}
