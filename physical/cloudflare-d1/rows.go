package cloudflare_d1

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/cloudflare/cloudflare-go/v6/d1"
	physical_utils "github.com/openbao/openbao/physical/utils"
)

type D1Rows struct {
	rows []interface{}
	pos  int
	err  error
}

type D1Row struct {
	rows physical_utils.Rows
}

func NewD1Rows(results *[]d1.QueryResult) *D1Rows {
	rows := []interface{}{}
	if results == nil {
		return &D1Rows{rows: rows, pos: -1}
	}
	for _, result := range *results {
		rows = append(rows, result.Results...)
	}
	return &D1Rows{rows: rows, pos: -1}
}

func (r *D1Rows) Next() bool {
	if r.err != nil {
		return false
	}
	if r.pos+1 >= len(r.rows) {
		return false
	}
	r.pos++
	return true
}

func (r *D1Rows) Scan(dest ...any) error {
	if r.pos < 0 || r.pos >= len(r.rows) {
		return physical_utils.ErrNoRows
	}
	values, err := rowValues(r.rows[r.pos])
	if err != nil {
		r.err = err
		return err
	}
	if len(dest) > len(values) {
		return fmt.Errorf("cloudflare d1 row has %d columns, scan requested %d", len(values), len(dest))
	}
	for i := range dest {
		if err := assignValue(dest[i], values[i]); err != nil {
			r.err = err
			return err
		}
	}
	return nil
}

func (r *D1Rows) Close() error { return nil }
func (r *D1Rows) Err() error   { return r.err }

func (r *D1Row) Scan(dest ...any) error {
	if !r.rows.Next() {
		if err := r.rows.Err(); err != nil {
			return err
		}
		return physical_utils.ErrNoRows
	}
	return r.rows.Scan(dest...)
}

func rowValues(row any) ([]any, error) {
	switch typed := row.(type) {
	case map[string]interface{}:
		if len(typed) != 1 {
			return nil, fmt.Errorf("cloudflare d1 row has %d columns; multi-column scans require ordered metadata", len(typed))
		}
		for _, value := range typed {
			return []any{value}, nil
		}
	case []any:
		return typed, nil
	}

	var decoded map[string]any
	data, err := json.Marshal(row)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, err
	}
	if len(decoded) != 1 {
		return nil, fmt.Errorf("cloudflare d1 row has %d columns; multi-column scans require ordered metadata", len(decoded))
	}
	for _, value := range decoded {
		return []any{value}, nil
	}
	return nil, physical_utils.ErrNoRows
}

func assignValue(dest any, value any) error {
	switch d := dest.(type) {
	case *string:
		switch v := value.(type) {
		case string:
			*d = v
		default:
			*d = fmt.Sprint(v)
		}
	case *[]byte:
		switch v := value.(type) {
		case []byte:
			*d = append((*d)[:0], v...)
		case string:
			if decoded, err := base64.StdEncoding.DecodeString(v); err == nil {
				*d = decoded
			} else {
				*d = []byte(v)
			}
		default:
			*d = []byte(fmt.Sprint(v))
		}
	case *int:
		i, err := toInt64(value)
		if err != nil {
			return err
		}
		*d = int(i)
	case *int64:
		i, err := toInt64(value)
		if err != nil {
			return err
		}
		*d = i
	case *float64:
		f, err := toFloat64(value)
		if err != nil {
			return err
		}
		*d = f
	case *bool:
		switch v := value.(type) {
		case bool:
			*d = v
		case string:
			parsed, err := strconv.ParseBool(v)
			if err != nil {
				return err
			}
			*d = parsed
		default:
			return fmt.Errorf("cannot scan %T into *bool", value)
		}
	default:
		return fmt.Errorf("unsupported scan destination %T", dest)
	}
	return nil
}

func toInt64(value any) (int64, error) {
	switch v := value.(type) {
	case int:
		return int64(v), nil
	case int64:
		return v, nil
	case float64:
		return int64(v), nil
	case string:
		return strconv.ParseInt(v, 10, 64)
	default:
		return 0, fmt.Errorf("cannot scan %T into integer", value)
	}
}

func toFloat64(value any) (float64, error) {
	switch v := value.(type) {
	case float64:
		return v, nil
	case int:
		return float64(v), nil
	case int64:
		return float64(v), nil
	case string:
		return strconv.ParseFloat(v, 64)
	default:
		return 0, fmt.Errorf("cannot scan %T into float64", value)
	}
}
