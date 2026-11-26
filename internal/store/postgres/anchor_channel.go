package postgres

import (
	"context"
	"crypto/rand"
	"errors"
	"iter"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/anchor"
)

func (s *Store) ChannelReport(ctx context.Context, query anchor.ChannelFilter) (anchor.ChannelTotals, error) {
	var result anchor.ChannelTotals
	err := s.pool.QueryRow(ctx, `WITH totals AS (
		SELECT count(DISTINCT u.user_id) AS users,
			count(DISTINCT u.user_id) FILTER(WHERE u.joined_at >= $2::timestamptz AND u.joined_at < $3::timestamptz) AS new_users,
			count(DISTINCT p.user_id) AS buyers,count(p.id) AS transactions,
			COALESCE(sum(p.points),0)::bigint AS points
		FROM anchor_channel_users u LEFT JOIN anchor_channel_spends p ON p.user_id=u.user_id
			AND ($2::timestamptz IS NULL OR p.spent_at >= $2) AND ($3::timestamptz IS NULL OR p.spent_at < $3)
		WHERE u.channel=$1
	)
	SELECT users,new_users,buyers,transactions,points,COALESCE(r.revenue_ratio,0)
	FROM anchor_channel_snapshot s CROSS JOIN totals
	LEFT JOIN anchor_channel_rates r ON r.channel=$1`, query.Channel, query.From, query.To).
		Scan(&result.Users, &result.NewUsers, &result.Buyers, &result.Transactions, &result.Points, &result.RevenueRatio)
	if errors.Is(err, pgx.ErrNoRows) {
		err = anchor.ErrChannelUnavailable
	}
	return result, err
}

func (s *Store) ImportLegacyChannels(ctx context.Context, users iter.Seq2[anchor.ChannelUser, error],
	spends iter.Seq2[anchor.ChannelSpend, error], rates iter.Seq2[anchor.ChannelRate, error]) error {
	batch := rand.Text()
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(167884962,53)`); err != nil {
			return err
		}
		for entry, err := range users {
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO anchor_channel_users(user_id,channel,joined_at,snapshot)
				VALUES($1,$2,$3,$4) ON CONFLICT(user_id) DO UPDATE SET
				channel=EXCLUDED.channel,joined_at=EXCLUDED.joined_at,snapshot=EXCLUDED.snapshot`,
				entry.ID, entry.Channel, entry.Joined, batch); err != nil {
				return err
			}
		}
		for entry, err := range spends {
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO anchor_channel_spends(id,user_id,points,spent_at,snapshot)
				VALUES($1,$2,$3,$4,$5) ON CONFLICT(id,user_id) DO UPDATE SET
				points=EXCLUDED.points,spent_at=EXCLUDED.spent_at,snapshot=EXCLUDED.snapshot`,
				entry.ID, entry.UserID, entry.Points, entry.Created, batch); err != nil {
				return err
			}
		}
		for entry, err := range rates {
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO anchor_channel_rates(channel,revenue_ratio,snapshot)
				VALUES($1,$2,$3) ON CONFLICT(channel) DO UPDATE SET
				revenue_ratio=EXCLUDED.revenue_ratio,snapshot=EXCLUDED.snapshot`, entry.Channel, entry.Ratio, batch); err != nil {
				return err
			}
		}
		for _, table := range []string{"anchor_channel_spends", "anchor_channel_users", "anchor_channel_rates"} {
			if _, err := tx.Exec(ctx, "DELETE FROM "+table+" WHERE snapshot<>$1", batch); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `INSERT INTO anchor_channel_snapshot(active,completed_at) VALUES(TRUE,statement_timestamp())
			ON CONFLICT(active) DO UPDATE SET completed_at=EXCLUDED.completed_at`)
		return err
	})
}
