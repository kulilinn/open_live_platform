package postgres

import (
	"context"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/anchor"
)

var _ anchor.TaskStore = (*Store)(nil)

type taskEvent struct {
	target anchor.TaskTarget
	key    string
	count  int64
}

func (s *Store) Tasks(ctx context.Context, owner int64, now time.Time) (anchor.TaskProgress, error) {
	var result anchor.TaskProgress
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		progress, err := taskProgress(ctx, tx, owner, anchor.TaskDay(now))
		if err != nil {
			return err
		}
		var gold int64
		if err := tx.QueryRow(ctx, `SELECT
			(COALESCE((SELECT sum(stars) FROM anchor_task_rewards WHERE anchor_id=$1),0)
			+COALESCE((SELECT stars FROM legacy_anchor_star_balances WHERE anchor_id=$1),0))::bigint`, owner).Scan(&gold); err != nil {
			return err
		}
		result = anchor.StarTasks(progress, gold)
		return nil
	})
	return result, err
}

func (s *Store) ReconcileTasks(ctx context.Context, owner int64, now time.Time) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockAnchorTasks(ctx, tx, owner); err != nil {
			return err
		}
		return awardAnchorTasks(ctx, tx, owner, now)
	})
}

func (s *Store) StarTaskStarted(ctx context.Context, owner int64) (*time.Time, error) {
	var first *time.Time
	err := s.pool.QueryRow(ctx, anchorTaskFirstAward, owner).Scan(&first)
	return first, err
}

const anchorTaskFirstAward = `SELECT LEAST((SELECT min(awarded_at) FROM anchor_task_rewards WHERE anchor_id=$1),
	(SELECT started_at FROM legacy_anchor_star_balances WHERE anchor_id=$1))`

func lockAnchorTasks(ctx context.Context, tx pgx.Tx, owner int64) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('anchor-task:' || $1::bigint::text,0))`, owner)
	return err
}

func recordAnchorTask(ctx context.Context, tx pgx.Tx, owner int64, at time.Time, events ...taskEvent) error {
	if len(events) == 0 {
		return nil
	}
	if err := lockAnchorTasks(ctx, tx, owner); err != nil {
		return err
	}
	day := anchor.TaskDay(at)
	var changed bool
	for _, event := range events {
		tag, err := tx.Exec(ctx, `INSERT INTO anchor_task_events(anchor_id,task_day,target,event_key,quantity,occurred_at)
			VALUES($1,$2::text::date,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, owner, day, event.target, event.key, event.count, at)
		if err != nil {
			return err
		}
		changed = changed || tag.RowsAffected() > 0
	}
	if changed {
		return awardAnchorTasks(ctx, tx, owner, at)
	}
	return nil
}

func taskProgress(ctx context.Context, tx pgx.Tx, owner int64, day string) ([anchor.TaskTargetCount]int64, error) {
	var progress [anchor.TaskTargetCount]int64
	rows, err := tx.Query(ctx, `SELECT target,sum(quantity)::bigint FROM anchor_task_events
		WHERE anchor_id=$1 AND task_day=$2::text::date GROUP BY target`, owner, day)
	if err != nil {
		return progress, err
	}
	defer rows.Close()
	for rows.Next() {
		var target int
		var count int64
		if err := rows.Scan(&target, &count); err != nil {
			return progress, err
		}
		progress[target] = count
	}
	return progress, rows.Err()
}

func awardAnchorTasks(ctx context.Context, tx pgx.Tx, owner int64, now time.Time) error {
	var first *time.Time
	if err := tx.QueryRow(ctx, anchorTaskFirstAward, owner).Scan(&first); err != nil {
		return err
	}
	if first != nil && !now.Before(first.Add(anchor.TaskEligibility)) {
		return nil
	}
	day := anchor.TaskDay(now)
	progress, err := taskProgress(ctx, tx, owner, day)
	if err != nil {
		return err
	}
	for _, task := range anchor.StarTasks(progress, 0).Tasks {
		if task.Progress < task.Condition {
			continue
		}
		if _, err := tx.Exec(ctx, `INSERT INTO anchor_task_rewards(anchor_id,task_day,task_id,stars,awarded_at)
			VALUES($1,$2::text::date,$3,$4,$5) ON CONFLICT DO NOTHING`, owner, day, task.ID, task.Reward, now); err != nil {
			return err
		}
	}
	return nil
}

func recordAnchorTaskGift(ctx context.Context, tx pgx.Tx, owner, sender int64, order string, rocket bool, at time.Time) error {
	if owner == sender {
		return nil
	}
	events := []taskEvent{{target: anchor.TaskGiftSender, key: strconv.FormatInt(sender, 10), count: 1}}
	if rocket {
		events = append(events, taskEvent{target: anchor.TaskRocket, key: order, count: 1})
	}
	return recordAnchorTask(ctx, tx, owner, at, events...)
}
