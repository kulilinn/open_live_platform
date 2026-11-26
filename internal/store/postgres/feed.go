package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/feed"
)

func (s *Store) FeedConfig(ctx context.Context, id int64) (feed.Config, error) {
	var c feed.Config
	err := s.pool.QueryRow(ctx, "SELECT dating,dated,welcome FROM message_config WHERE user_id=$1", id).Scan(&c.Dating, &c.Dated, &c.Welcome)
	if errors.Is(err, pgx.ErrNoRows) {
		return feed.Config{Dated: true}, nil
	}
	return c, err
}
func (s *Store) SetFeedConfig(ctx context.Context, id int64, c feed.Config) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO message_config(user_id,dating,dated) VALUES($1,$2,$3)
		ON CONFLICT(user_id) DO UPDATE SET dating=excluded.dating,dated=excluded.dated`, id, c.Dating, c.Dated)
	return err
}
func (s *Store) FeedEntries(ctx context.Context, id int64) ([]feed.Entry, error) {
	rows, err := s.pool.Query(ctx, `SELECT peer_id,data,unreads,updated_at FROM message_inbox WHERE owner_id=$1 AND NOT hidden
		ORDER BY updated_at DESC,peer_id DESC LIMIT 512`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[feed.Entry])
}
func (s *Store) SetFeedWelcome(ctx context.Context, id int64, welcome string) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO message_config(user_id,dating,dated,welcome) VALUES($1,false,true,$2)
		ON CONFLICT(user_id) DO UPDATE SET welcome=excluded.welcome`, id, welcome)
	return err
}
func (s *Store) FeedUnread(ctx context.Context, id int64) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, "SELECT coalesce(sum(unreads),0) FROM message_inbox WHERE owner_id=$1 AND NOT hidden", id).Scan(&n)
	return n, err
}
func (s *Store) SetFeedEntry(ctx context.Context, uid, peer int64, data string) error {
	// Always lock the two rows in recipient ID order, including simultaneous replies.
	_, err := s.pool.Exec(ctx, `INSERT INTO message_inbox(owner_id,peer_id,data,unreads)
		SELECT owner,peer,$3,1 FROM (VALUES($1::bigint,$2::bigint),($2,$1)) AS pairs(owner,peer) ORDER BY owner
		ON CONFLICT(owner_id,peer_id) DO UPDATE SET data=excluded.data,hidden=false,updated_at=now(),
		unreads=message_inbox.unreads+(excluded.owner_id=$2)::int`, uid, peer, data)
	return err
}
func (s *Store) MarkFeedEntry(ctx context.Context, uid, peer int64, hide bool) error {
	_, err := s.pool.Exec(ctx, "UPDATE message_inbox SET hidden=hidden OR $3,unreads=CASE WHEN $3 THEN unreads ELSE 0 END WHERE owner_id=$1 AND peer_id=$2", uid, peer, hide)
	return err
}
