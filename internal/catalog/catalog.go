// Package catalog reads public reference data shared by the app directory, help and release APIs.
package catalog

import (
	"context"
	"encoding/json"
	"strconv"
)

type Row map[string]json.RawMessage

func (r Row) String(key string) string { var s string; _ = json.Unmarshal(r[key], &s); return s }
func (r Row) Int(key string) int64     { n, _ := strconv.ParseInt(string(r[key]), 10, 64); return n }

type Store interface {
	CatalogRows(ctx context.Context, section string) ([]Row, error)
}
type Snapshot map[string][]Row

func (s Snapshot) CatalogRows(_ context.Context, section string) ([]Row, error) {
	return s[section], nil
}
