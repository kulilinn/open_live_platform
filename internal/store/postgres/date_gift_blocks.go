package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/date"
	"open_live_platform/internal/gift"
)

var _ gift.BlockStore = (*Store)(nil)

func (s *Store) BlockDateGifts(ctx context.Context, channel, actor int64, ids []int64) error {
	return s.dateChange(ctx, func(tx pgx.Tx) error {
		session, err := loadDateSession(ctx, tx, channel)
		if err != nil {
			return err
		}
		if session.AnchorID != actor {
			return date.ErrForbidden
		}
		if !session.Active(time.Now()) {
			return date.ErrClosed
		}
		_, err = tx.Exec(ctx, `INSERT INTO date_gift_blocks(channel,gift_id)
			SELECT $1,UNNEST($2::bigint[]) ON CONFLICT DO NOTHING`, channel, ids)
		return err
	})
}

func (s *Store) BlockedDateGifts(ctx context.Context, channel int64) (map[int64]struct{}, error) {
	rows, err := s.pool.Query(ctx, `SELECT gift_id FROM date_gift_blocks WHERE channel=$1`, channel)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[int64]struct{})
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		result[id] = struct{}{}
	}
	return result, rows.Err()
}

func dateGiftBlocked(ctx context.Context, tx pgx.Tx, channel, giftID int64) (bool, error) {
	var blocked bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM date_gift_blocks WHERE channel=$1 AND gift_id=$2)`, channel, giftID).Scan(&blocked)
	return blocked, err
}
