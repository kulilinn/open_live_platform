package postgres

import (
	"context"
	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/feed"
)

const shareColumns = "id,user_id,content,comments,photo,likes,status,created_at,updated_at"

func scanShare(row pgx.Row) (feed.Share, error) {
	var v feed.Share
	err := row.Scan(&v.ID, &v.UserID, &v.Content, &v.Comments, &v.Photo, &v.Likes, &v.Status, &v.Created, &v.Updated)
	v.Created, v.Updated = v.Created.UTC(), v.Updated.UTC()
	return v, err
}
func (s *Store) CreateShare(ctx context.Context, uid int64, content, photo string) (feed.Share, error) {
	return scanShare(s.pool.QueryRow(ctx, "INSERT INTO public_shares(user_id,content,photo) VALUES($1,$2,$3) RETURNING "+shareColumns, uid, content, photo))
}
func (s *Store) OwnsShare(ctx context.Context, uid, id int64) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM public_shares WHERE id=$2 AND user_id=$1 AND status>0)", uid, id).Scan(&ok)
	return ok, err
}
func (s *Store) ChangeShare(ctx context.Context, uid, id int64, content, photo string, remove bool) error {
	tag, err := s.pool.Exec(ctx, `UPDATE public_shares SET content=$3,photo=CASE WHEN $5 THEN photo ELSE $4 END,
		status=CASE WHEN $5 THEN 0 ELSE 2 END,updated_at=now() WHERE user_id=$1 AND id=$2 AND status>0`, uid, id, content, photo, remove)
	if err == nil && tag.RowsAffected() == 0 {
		return feed.ErrShareMissing
	}
	return err
}
func (s *Store) ListShares(ctx context.Context, offset int64, hot bool, owner int64) ([]feed.Share, error) {
	order := "created_at DESC,id DESC"
	if hot {
		order = "likes DESC," + order
	}
	rows, err := s.pool.Query(ctx, "SELECT "+shareColumns+" FROM public_shares WHERE status>0 AND ($1::bigint=0 OR user_id=$1) ORDER BY "+order+" LIMIT 100 OFFSET $2", owner, offset)
	if err != nil {
		return nil, err
	}
	return pgx.AppendRows([]feed.Share{}, rows, func(r pgx.CollectableRow) (feed.Share, error) { return scanShare(r) })
}
