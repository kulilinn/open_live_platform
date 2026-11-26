package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/relay"
)

func (s *Store) SetRustControlGrant(ctx context.Context, appKey string, controller int64, grant relay.RustControlGrant, ttl time.Duration) error {
	if ttl == 0 {
		_, err := s.pool.Exec(ctx, `DELETE FROM relay_control_grants WHERE app_key=$1 AND controller_id=$2 AND controlled_id=$3`, appKey, controller, grant.ControlledID)
		return err
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO relay_control_grants(app_key,controller_id,controlled_id,duplex,granted_at,expires_at)
		VALUES($1,$2,$3,$4,statement_timestamp(),statement_timestamp()+make_interval(secs => $5))
		ON CONFLICT (app_key,controller_id,controlled_id) DO UPDATE SET
		duplex=EXCLUDED.duplex,granted_at=EXCLUDED.granted_at,expires_at=EXCLUDED.expires_at`,
		appKey, controller, grant.ControlledID, grant.Duplex, ttl.Seconds())
	return err
}

func (s *Store) LookupRustControlGrant(ctx context.Context, appKey string, controller, requested int64) (relay.RustControlGrant, bool, error) {
	var grant relay.RustControlGrant
	err := s.pool.QueryRow(ctx, `SELECT controlled_id,duplex FROM relay_control_grants
		WHERE app_key=$1 AND controller_id=$2 AND ($3::bigint=0 OR controlled_id=$3)
		AND expires_at>statement_timestamp() ORDER BY granted_at DESC, controlled_id LIMIT 1`, appKey, controller, requested).
		Scan(&grant.ControlledID, &grant.Duplex)
	if errors.Is(err, pgx.ErrNoRows) {
		return relay.RustControlGrant{}, false, nil
	}
	return grant, err == nil, err
}
