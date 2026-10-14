package cloudflare_d1

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/cloudflare/cloudflare-go/v6/option"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

const (
	testAccountID  = "test-account"
	testDatabaseID = "test-database"
	testQueryPath  = "/accounts/" + testAccountID + "/d1/database/" + testDatabaseID + "/query"
)

// fakeD1 serves the D1 REST query endpoint from a local SQLite database,
// returning rows as column-keyed objects like the real API.
type fakeD1 struct {
	db *sql.DB
}

func newFakeD1(t *testing.T) *fakeD1 {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "d1.db"))
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return &fakeD1{db: db}
}

func (f *fakeD1) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != testQueryPath {
		writeD1Error(w, http.StatusNotFound, "unknown route")
		return
	}
	var body struct {
		SQL    string `json:"sql"`
		Params []any  `json:"params"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.UseNumber()
	if err := decoder.Decode(&body); err != nil {
		writeD1Error(w, http.StatusBadRequest, err.Error())
		return
	}
	args := make([]any, len(body.Params))
	for i, param := range body.Params {
		if n, ok := param.(json.Number); ok {
			if v, err := n.Int64(); err == nil {
				param = v
			} else if v, err := n.Float64(); err == nil {
				param = v
			}
		}
		args[i] = param
	}
	rows, err := f.db.QueryContext(r.Context(), body.SQL, args...)
	if err != nil {
		writeD1Error(w, http.StatusBadRequest, err.Error())
		return
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		writeD1Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	results := []map[string]any{}
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			writeD1Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		row := make(map[string]any, len(columns))
		for i, column := range columns {
			if b, ok := values[i].([]byte); ok {
				values[i] = string(b)
			}
			row[column] = values[i]
		}
		results = append(results, row)
	}
	if err := rows.Err(); err != nil {
		writeD1Error(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true, "errors": []any{}, "messages": []any{},
		"result": []any{map[string]any{"results": results, "success": true, "meta": map[string]any{}}},
	})
}

func writeD1Error(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"success": false, "messages": []any{}, "result": nil,
		"errors": []any{map[string]any{"code": 7500, "message": message}},
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(body)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// newTestDB returns a client for server that goes through the production
// client construction, pointed at the test server.
func newTestDB(t *testing.T, server *httptest.Server, policy requestPolicy) *DB {
	t.Helper()
	creds := D1ConnectionCredentials{AccountID: testAccountID, APIKey: "test-token", DatabaseID: testDatabaseID}
	return newDB(creds, policy, nil, option.WithBaseURL(server.URL))
}
