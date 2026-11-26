package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/notice"
)

func (s *Store) AddNotice(ctx context.Context, n notice.Notice) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO notices(id,notice_target,notice_type,receive_id,sender_avatar,content,biz_id,create_time)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT (id) DO NOTHING`, n.ID, n.Target, n.Type, n.UserID, n.Avatar, n.Content, n.ObjectID, n.CreatedAt)
	return err
}

func (s *Store) Notices(ctx context.Context, userID int64, page, size int32) (notice.Page, error) {
	result := notice.Page{Current: page, Size: size}
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM notices WHERE receive_id=$1", userID).Scan(&result.Total); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id,notice_target,notice_type,receive_id,sender_avatar,content,biz_id,status,create_time
			FROM notices WHERE receive_id=$1 ORDER BY create_time DESC,id DESC LIMIT $2 OFFSET $3`, userID, size, int64(page-1)*int64(size))
		if err != nil {
			return err
		}
		result.List, err = pgx.CollectRows(rows, pgx.RowToStructByPos[notice.Notice])
		return err
	})
	return result, err
}

func (s *Store) ReadNotices(ctx context.Context, userID int64) error {
	_, err := s.pool.Exec(ctx, "UPDATE notices SET status=2 WHERE receive_id=$1 AND status=1", userID)
	return err
}

func (s *Store) UnreadNotices(ctx context.Context, userID int64) (int64, error) {
	var count int64
	err := s.pool.QueryRow(ctx, "SELECT count(*) FROM notices WHERE receive_id=$1 AND status=1", userID).Scan(&count)
	return count, err
}
