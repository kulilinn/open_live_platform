package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/subscription"
)

const subscriptionGroupFields = `g.id,g.kind,g.name,ARRAY(SELECT person_id FROM subscription_group_members m
	WHERE m.group_id=g.id ORDER BY added_at,person_id)`

func scanSubscriptionGroup(row pgx.Row) (subscription.Group, error) {
	var group subscription.Group
	err := row.Scan(&group.ID, &group.Kind, &group.Name, &group.Users)
	if errors.Is(err, pgx.ErrNoRows) {
		err = subscription.ErrNotFound
	}
	return group, err
}

func (s *Store) SubscriptionGroups(ctx context.Context, owner int64, kind int) ([]subscription.Group, error) {
	if kind == subscription.CustomGroup {
		return readSubscriptionGroups(ctx, s.pool, owner, kind)
	}
	var result []subscription.Group
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockSubscriptionDirectory(ctx, tx, owner); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO subscription_groups(owner_id,kind,name) VALUES($1,$2,'') ON CONFLICT DO NOTHING`, owner, kind)
		if err != nil {
			return err
		}
		result, err = readSubscriptionGroups(ctx, tx, owner, kind)
		return err
	})
	return result, err
}

type subscriptionGroupQuery interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func readSubscriptionGroups(ctx context.Context, q subscriptionGroupQuery, owner int64, kind int) ([]subscription.Group, error) {
	rows, err := q.Query(ctx, `SELECT `+subscriptionGroupFields+` FROM subscription_groups g WHERE g.owner_id=$1 AND g.kind=$2 ORDER BY g.id`, owner, kind)
	if err != nil {
		return nil, err
	}
	return pgx.AppendRows([]subscription.Group{}, rows, func(row pgx.CollectableRow) (subscription.Group, error) {
		return scanSubscriptionGroup(row)
	})
}

func (s *Store) SubscriptionGroup(ctx context.Context, owner, id int64) (subscription.Group, error) {
	return scanSubscriptionGroup(s.pool.QueryRow(ctx, `SELECT `+subscriptionGroupFields+` FROM subscription_groups g WHERE g.owner_id=$1 AND g.id=$2`, owner, id))
}

func (s *Store) CreateSubscriptionGroup(ctx context.Context, owner int64, name string) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO subscription_groups(owner_id,kind,name) VALUES($1,3,$2)`, owner, name)
	return err
}

func (s *Store) ChangeSubscriptionGroup(ctx context.Context, owner, id int64, change subscription.GroupChange) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockSubscriptionDirectory(ctx, tx, owner); err != nil {
			return err
		}
		if err := lockSubscriptionGroup(ctx, tx, owner, id); err != nil {
			return err
		}
		switch {
		case change.Delete:
			_, err := tx.Exec(ctx, `DELETE FROM subscription_groups WHERE id=$1 AND owner_id=$2`, id, owner)
			return err
		case change.Clear:
			_, err := tx.Exec(ctx, `DELETE FROM subscription_group_members WHERE group_id=$1`, id)
			return err
		default:
			_, err := tx.Exec(ctx, `UPDATE subscription_groups SET name=$3 WHERE id=$1 AND owner_id=$2`, id, owner, change.Name)
			return err
		}
	})
}

func lockSubscriptionGroup(ctx context.Context, tx pgx.Tx, owner, id int64) error {
	err := tx.QueryRow(ctx, `SELECT id FROM subscription_groups WHERE id=$1 AND owner_id=$2 FOR UPDATE`, id, owner).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return subscription.ErrNotFound
	}
	return err
}

func lockSubscriptionDirectory(ctx context.Context, tx pgx.Tx, owner int64) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('subscription-directory:' || $1::bigint::text,0))`, owner)
	return err
}

func (s *Store) SavedSubscriptionUsers(ctx context.Context, owner int64, list int) ([]int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT person_id FROM subscription_saved_users WHERE owner_id=$1 AND kind=$2 ORDER BY person_id`, owner, list)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

func (s *Store) ClearSubscriptionList(ctx context.Context, owner int64, list int) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockSubscriptionDirectory(ctx, tx, owner); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM subscription_saved_users WHERE owner_id=$1 AND kind=$2`, owner, list)
		return err
	})
}

func (s *Store) ChangeSubscriptionMember(ctx context.Context, owner int64, change subscription.MemberChange) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockSubscriptionDirectory(ctx, tx, owner); err != nil {
			return err
		}
		if change.GroupID != 0 {
			if err := lockSubscriptionGroup(ctx, tx, owner, change.GroupID); err != nil {
				return err
			}
		}
		person := change.PersonID
		if change.Add && change.List != subscription.Blacklist {
			var blocked bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM subscription_saved_users WHERE owner_id=$1 AND person_id=$2 AND kind=3)`,
				owner, person).Scan(&blocked); err != nil {
				return err
			}
			if blocked {
				return subscription.ErrForbidden
			}
		}
		if change.List != 0 {
			query := `DELETE FROM subscription_saved_users WHERE owner_id=$1 AND person_id=$2 AND kind=$3`
			if change.Add {
				query = `INSERT INTO subscription_saved_users(owner_id,person_id,kind) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`
			}
			if _, err := tx.Exec(ctx, query, owner, person, change.List); err != nil {
				return err
			}
		}
		if change.Add && change.List == subscription.Blacklist {
			if _, err := tx.Exec(ctx, `DELETE FROM subscription_saved_users WHERE owner_id=$1 AND person_id=$2 AND kind=2`, owner, person); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `DELETE FROM subscription_group_members m USING subscription_groups g
				WHERE m.group_id=g.id AND g.owner_id=$1 AND m.person_id=$2`, owner, person)
			return err
		}
		if change.GroupID != 0 {
			query := `DELETE FROM subscription_group_members WHERE group_id=$1 AND person_id=$2`
			if change.Add {
				query = `INSERT INTO subscription_group_members(group_id,person_id) VALUES($1,$2) ON CONFLICT DO NOTHING`
			}
			_, err := tx.Exec(ctx, query, change.GroupID, person)
			return err
		}
		return nil
	})
}

func (s *Store) ReportSubscription(ctx context.Context, report subscription.Report) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO subscription_reports(reporter_id,person_id,post_id,kind,urls,remark)
		VALUES($1,NULLIF($2,0),NULLIF($3,0),$4,$5,$6)`, report.Reporter, report.PersonID, report.PostID, report.Kind, report.URLs, report.Remark)
	return err
}

func (s *Store) SubscriptionExperience(ctx context.Context, fanID, anchorID int64) (platform, fan int64, err error) {
	err = s.pool.QueryRow(ctx, `WITH spending AS (
		SELECT p.anchor_id,o.points FROM subscription_orders o JOIN subscription_plans p ON p.id=o.plan_id
		WHERE o.status=1 AND o.provider_order_id IS NOT NULL AND o.fan_id=$1
		UNION ALL SELECT receiver_id,cost FROM gift_orders WHERE state=1 AND provider_order_id IS NOT NULL AND sender_id=$1
		UNION ALL SELECT p.owner_id,o.points FROM anchor_post_orders o JOIN anchor_posts p ON p.id=o.post_id
		WHERE o.state=1 AND o.actor_id=$1
	)
	SELECT COALESCE(sum(points),0)::bigint,
		CASE WHEN EXISTS(SELECT 1 FROM subscription_members m JOIN subscription_plans p ON p.id=m.plan_id
			WHERE m.fan_id=$1 AND m.anchor_id=$2 AND COALESCE(m.legacy_level,p.plan_type-2)>0)
		THEN COALESCE(sum(points) FILTER (WHERE anchor_id=$2),0)::bigint ELSE 0 END FROM spending`, fanID, anchorID).Scan(&platform, &fan)
	return platform, fan, err
}
