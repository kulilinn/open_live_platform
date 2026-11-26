package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/upload"
)

func (s *Store) ReserveIssuedPass(ctx context.Context, record upload.PassIssue) (bool, error) {
	tag, err := s.pool.Exec(ctx, `INSERT INTO wallet_passes(id,user_id,issued_at,expires_at,token_expires_at)
		VALUES($1,$2,$3,$4,$5) ON CONFLICT(id) DO NOTHING`, record.ID, record.UserID, record.IssuedAt, record.ExpiresAt, record.TokenExpiresAt)
	return tag.RowsAffected() == 1, err
}

func (s *Store) PublishIssuedPass(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `UPDATE wallet_passes SET published_at=COALESCE(published_at,statement_timestamp()),
		expires_at=CASE WHEN published_at IS NULL THEN statement_timestamp()+interval '24 hours' ELSE expires_at END
		WHERE id=$1 AND expires_at>statement_timestamp()`, id)
	if err == nil && tag.RowsAffected() != 1 {
		return upload.ErrPassNotFound
	}
	return err
}

func (s *Store) FindIssuedPass(ctx context.Context, id int64) (upload.PassIssue, error) {
	var record upload.PassIssue
	err := s.pool.QueryRow(ctx, `SELECT id,user_id,issued_at,expires_at,token_expires_at FROM wallet_passes
		WHERE id=$1 AND published_at IS NOT NULL AND expires_at>statement_timestamp()`, id).
		Scan(&record.ID, &record.UserID, &record.IssuedAt, &record.ExpiresAt, &record.TokenExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = upload.ErrPassNotFound
	}
	return record, err
}
