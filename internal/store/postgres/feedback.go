package postgres

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/feedback"
)

func (s *Store) ReserveFeedback(ctx context.Context, r feedback.Record) (id int64, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT id FROM users WHERE id=$1 FOR UPDATE", r.UserID); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `INSERT INTO feedback(user_id,feedback_type,content,attachment,notification_status)
			SELECT $1,$2,$3,$4,$5 WHERE NOT EXISTS(SELECT 1 FROM feedback WHERE user_id=$1 AND created_at>now()-interval '1 minute')
			RETURNING id`, r.UserID, r.Type, r.Content, r.Attachment, r.Status).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return feedback.ErrTooOften
		}
		return err
	})
	return
}
func (s *Store) DiscardFeedback(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, "DELETE FROM feedback WHERE id=$1", id)
	return err
}
func (s *Store) FinishFeedback(ctx context.Context, id int64, status int) error {
	_, err := s.pool.Exec(ctx, "UPDATE feedback SET notification_status=$2 WHERE id=$1", id, status)
	return err
}
