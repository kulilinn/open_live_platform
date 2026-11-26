package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"iter"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/go-sql-driver/mysql"
	"open_live_platform/internal/store/postgres"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context) error {
	sourceDSN, targetDSN := os.Getenv("LIVEMEDIA_MYSQL_DSN"), os.Getenv("DATABASE_URL")
	if sourceDSN == "" || targetDSN == "" {
		return errors.New("set LIVEMEDIA_MYSQL_DSN and DATABASE_URL")
	}
	config, err := mysql.ParseDSN(sourceDSN)
	if err != nil {
		return errors.New("invalid LIVEMEDIA_MYSQL_DSN")
	}
	config.ParseTime, config.Loc = true, time.UTC
	connector, err := mysql.NewConnector(config)
	if err != nil {
		return errors.New("invalid LIVEMEDIA_MYSQL_DSN")
	}
	source := sql.OpenDB(connector)
	defer source.Close()
	snapshot, err := source.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return fmt.Errorf("read MySQL subscription snapshot: %w", err)
	}
	defer snapshot.Rollback()
	target, err := postgres.Open(ctx, targetDSN)
	if err != nil {
		return fmt.Errorf("open PostgreSQL: %w", err)
	}
	defer target.Shutdown()
	counts, err := target.ImportLegacySubscriptions(ctx,
		queryRows(ctx, snapshot, plansQuery, readPlan), queryRows(ctx, snapshot, membershipsQuery, readMembership),
		queryRows(ctx, snapshot, settlementsQuery, readSettlement))
	if err != nil {
		return err
	}
	fmt.Printf("plans=%d memberships=%d settlements=%d linked_settlements=%d existing=%d skipped=%d\n",
		counts.Plans, counts.Memberships, counts.Settlements, counts.LinkedSettlements, counts.Existing, counts.Skipped)
	return nil
}

const plansQuery = `SELECT id,anchor_id,is_pub,is_del,is_promotion,ty,buddle,url,remark,price,promotion,create_at,update_at,name
	FROM subscription_plan ORDER BY id`

const membershipsQuery = `SELECT id,uid,anchor_id,plan_id,buddle,create_at,expire_at,is_promotion_purchased,promotion_expire_at,
	subscription_level,cancel_at,reason FROM subscription_user_sub ORDER BY uid,plan_id,id`

// Settle order subjects are display names. They cannot establish which plan
// was purchased without the matching, typed points transaction.
const settlementsQuery = `SELECT s.id,s.no,s.user_id,s.anchor_id,s.settle_type,s.settle_ld,s.expend_point,s.point_transaction_id,
	s.point_transaction_no,s.subject,s.bad_debt_point,s.bad_debt_ld,s.status,s.settle_time,s.actual_settle_ld,s.settle_config_id,
	COALESCE(CAST(s.settle_ratio AS CHAR),''),s.create_time,s.update_time,p.id
	FROM settle_orders s LEFT JOIN point_transactions t ON t.id=s.point_transaction_id AND t.no=s.point_transaction_no
		AND t.user_id=s.user_id AND t.anchor_id=s.anchor_id AND t.income_or_expenses=-1 AND t.total_point=s.expend_point AND t.total_point>0
		AND ((s.settle_type=6 AND t.transaction_type=3) OR (s.settle_type=7 AND t.transaction_type=10))
	LEFT JOIN subscription_plan p ON p.id=t.transaction_order_id AND p.anchor_id=s.anchor_id AND p.ty IN (3,4)
	WHERE s.settle_type IN (6,7) ORDER BY s.id`

func queryRows[T any](ctx context.Context, snapshot *sql.Tx, query string, read func(*sql.Rows) (T, error)) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		var zero T
		rows, err := snapshot.QueryContext(ctx, query)
		if err != nil {
			yield(zero, err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			value, err := read(rows)
			if !yield(value, err) || err != nil {
				return
			}
		}
		if err := rows.Err(); err != nil {
			yield(zero, err)
		}
	}
}

func readPlan(rows *sql.Rows) (postgres.LegacySubscriptionPlan, error) {
	var p postgres.LegacySubscriptionPlan
	err := rows.Scan(&p.ID, &p.AnchorID, &p.Published, &p.Deleted, &p.Promotion, &p.Type, &p.Bundle, &p.URL, &p.Remark, &p.Price, &p.Promote,
		&p.CreatedAt, &p.UpdatedAt, &p.Name)
	return p, err
}

func readMembership(rows *sql.Rows) (postgres.LegacySubscriptionMember, error) {
	var m postgres.LegacySubscriptionMember
	err := rows.Scan(&m.ID, &m.FanID, &m.AnchorID, &m.PlanID, &m.Bundle, &m.CreatedAt, &m.ExpiresAt, &m.PromotionPurchased,
		&m.PromotionExpiresAt, &m.Level, &m.CancelledAt, &m.Reason)
	return m, err
}

func readSettlement(rows *sql.Rows) (postgres.LegacySubscriptionSettlement, error) {
	var p postgres.LegacySubscriptionSettlement
	err := rows.Scan(&p.ID, &p.No, &p.FanID, &p.AnchorID, &p.Type, &p.SettleLD, &p.Points, &p.TransactionID, &p.TransactionNo,
		&p.Subject, &p.BadDebtPoints, &p.BadDebtLD, &p.Status, &p.SettleTime, &p.ActualSettleLD, &p.SettleConfigID, &p.SettleRatio,
		&p.CreatedAt, &p.UpdatedAt, &p.VerifiedPlanID)
	return p, err
}
