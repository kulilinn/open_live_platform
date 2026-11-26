package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/admin"
	"open_live_platform/internal/payment"
)

func (s *Store) ReserveFinanceAttempt(ctx context.Context, actor admin.Principal, input admin.FinanceIntent) (admin.FinanceAttempt, bool, error) {
	var attempt admin.FinanceAttempt
	if actor.Area != admin.OperationArea || actor.Tenant != admin.FinanceTenant || actor.ID <= 0 {
		return attempt, false, admin.ErrBackofficeForbidden
	}
	err := s.pool.QueryRow(ctx, `INSERT INTO finance_payment_attempts(action,request_key,request_hash,request,user_id,operator_id,operator_name,point_delta,reason)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT(action,request_key) DO NOTHING RETURNING id,state`,
		input.Action, input.Key, input.Hash[:], input.Request, input.UserID, actor.ID, actor.Name(), input.Delta, input.Reason).Scan(&attempt.ID, &attempt.State)
	if err == nil {
		return attempt, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return attempt, false, err
	}
	var expected []byte
	err = s.pool.QueryRow(ctx, `SELECT id,state,receipt,request_hash FROM finance_payment_attempts WHERE action=$1 AND request_key=$2`, input.Action, input.Key).
		Scan(&attempt.ID, &attempt.State, &attempt.Receipt, &expected)
	if err == nil && !bytes.Equal(expected, input.Hash[:]) {
		err = admin.ErrFinanceConflict
	}
	return attempt, false, err
}

func (s *Store) CompleteFinanceAttempt(ctx context.Context, attemptID int64, state string, receipt json.RawMessage, updates []payment.Receipt) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var action, operatorName string
		var spender, operatorID int64
		err := tx.QueryRow(ctx, `UPDATE finance_payment_attempts SET state=$2,receipt=$3,finished_at=statement_timestamp()
			WHERE id=$1 AND state='reserved' RETURNING action,user_id,operator_id,operator_name`, attemptID, state, receipt).
			Scan(&action, &spender, &operatorID, &operatorName)
		if err != nil {
			return err
		}
		if state == admin.FinanceConfirmed && action == "bad_debt" {
			for _, item := range updates {
				if err := verifyFinanceDebtOwner(ctx, tx, spender, item); err != nil {
					return err
				}
				tag, err := tx.Exec(ctx, `INSERT INTO finance_bad_debt_receipts AS old (provider_order_id,provider_order_no,spender_id,charged_points,bad_debt_points,finance_attempt_id)
					VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(provider_order_id) DO UPDATE SET
					bad_debt_points=excluded.bad_debt_points,finance_attempt_id=excluded.finance_attempt_id,observed_at=statement_timestamp()
					WHERE (old.provider_order_no,old.spender_id,old.charged_points)=(excluded.provider_order_no,excluded.spender_id,excluded.charged_points)`,
					item.OrderID, item.OrderNo, spender, item.TotalPoints, item.BadDebt, attemptID)
				if err != nil {
					return err
				}
				if tag.RowsAffected() == 0 {
					return admin.ErrFinanceConflict
				}
			}
		}
		_, err = tx.Exec(ctx, `INSERT INTO backoffice_logs(area,tenant,operator_id,operator_name,path,target,action,reason)
			VALUES('admin','finance-admin',$1,$2,'/api/finance',$3,$4,$5)`, operatorID, operatorName,
			strconv.FormatInt(attemptID, 10), "finance_"+action, state)
		return err
	})
}

func verifyFinanceDebtOwner(ctx context.Context, tx pgx.Tx, spender int64, receipt payment.Receipt) error {
	var mismatched bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM (
			SELECT sender_id AS spender,receipt->>'order_no' AS number,cost AS amount FROM gift_orders WHERE state=1 AND provider_order_id=$1
			UNION ALL SELECT sender_id,order_data#>>'{Receipt,order_no}',(order_data->>'Cost')::bigint FROM date_gift_orders WHERE state=1 AND provider_id=$1
			UNION ALL SELECT user_id,order_data#>>'{Receipt,order_no}',(order_data->>'Price')::bigint FROM room_wheel_orders WHERE state=1 AND provider_id=$1
			UNION ALL SELECT actor_id,provider_no,points FROM anchor_post_orders WHERE state=1 AND provider_id=$1
			UNION ALL SELECT fan_id,provider_order_no,points FROM subscription_orders WHERE status=1 AND provider_order_id=$1
			UNION ALL SELECT owner_id,record->>'no',(record->>'total_point')::bigint FROM legacy_business_history
			 WHERE source_table='point_transactions' AND source_id=$1::bigint::text AND (record->>'income_or_expenses')::smallint=-1
		) ledger WHERE (ledger.spender,ledger.number,ledger.amount) IS DISTINCT FROM ($2::bigint,$3::text,$4::bigint)
	)`, receipt.OrderID, spender, receipt.OrderNo, receipt.TotalPoints).Scan(&mismatched)
	if err == nil && mismatched {
		return admin.ErrFinanceConflict
	}
	return err
}

func (s *Store) FinanceOrderOwner(ctx context.Context, order string, user int64) (bool, error) {
	var known bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM payment_large_order_attempts WHERE user_id=$1 AND provider_order=$2
		UNION ALL SELECT 1 FROM legacy_business_history WHERE source_table='pay_orders' AND owner_id=$1 AND record->>'no'=$2
		UNION ALL SELECT 1 FROM finance_payment_attempts WHERE action='large_credit' AND state='confirmed' AND user_id=$1 AND request_key=$2
	)`, user, order).Scan(&known)
	return known, err
}

func (s *Store) FinancePointLog(ctx context.Context, q admin.FinanceReadQuery) ([]admin.FinancePointLog, int64, error) {
	const relation = `FROM (
		SELECT id,user_id::text AS users,point_delta AS point,operator_id AS actor,operator_name AS name,finished_at AS ts,reason
		FROM finance_payment_attempts WHERE state='confirmed' AND point_delta<>0
		UNION ALL SELECT h.source_id::bigint,j.users,j.point,h.owner_id,j.create_by_name,j.create_time AT TIME ZONE 'UTC',j.reason
		FROM legacy_business_history h CROSS JOIN LATERAL jsonb_to_record(h.record)
			j(users text,point bigint,create_by_name text,create_time timestamp,reason text)
		WHERE h.source_table='admin_modify_point_log'
	) entries WHERE ($1::timestamptz IS NULL OR ts>=$1) AND ($2::timestamptz IS NULL OR ts<=$2)`
	return readBackofficePage(ctx, s, q.PageRequest, "users,point,actor,name,ts,reason", relation, "ts DESC,id DESC", []any{q.From, q.To},
		func(row pgx.CollectableRow) (admin.FinancePointLog, error) {
			var result admin.FinancePointLog
			err := row.Scan(&result.Users, &result.Points, &result.OperatorID, &result.Operator, &result.CreatedAt, &result.Reason)
			result.CreatedAt = result.CreatedAt.UTC()
			return result, err
		})
}

func (s *Store) FinancePurchaseStats(ctx context.Context, q admin.FinanceReadQuery) ([]admin.FinanceDailyPurchase, int64, error) {
	const relation = `FROM legacy_business_history h CROSS JOIN LATERAL jsonb_to_record(h.record) j
		(dau bigint,first_purchase_num bigint,purchase_user_num bigint,point_total bigint,consume_point_total bigint,platform_point_total bigint,
		pay_amount numeric,receivable_amount numeric,create_time text)
		WHERE h.source_table='order_statistic_data' AND ($1::text IS NULL OR j.create_time>=$1) AND ($2::text IS NULL OR j.create_time<=$2)`
	const columns = `h.source_id::bigint,COALESCE(j.dau,0),j.first_purchase_num,j.purchase_user_num,
		j.point_total,j.consume_point_total,j.platform_point_total,j.pay_amount::text,j.receivable_amount::text,j.create_time`
	var from, to any
	if q.From != nil && q.To != nil {
		from, to = q.From.UTC().Format("2006-01-02"), q.To.UTC().Format("2006-01-02")
	}
	return readBackofficePage(ctx, s, q.PageRequest, columns, relation, "j.create_time DESC,h.source_id::bigint DESC", []any{from, to},
		func(row pgx.CollectableRow) (admin.FinanceDailyPurchase, error) {
			var result admin.FinanceDailyPurchase
			err := row.Scan(&result.ID, &result.DAU, &result.FirstPurchase, &result.Purchasers, &result.PointTotal, &result.SpendTotal,
				&result.PlatformTotal, &result.Amount, &result.Receivable, &result.Day)
			return result, err
		})
}
