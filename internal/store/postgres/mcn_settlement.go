package postgres

import (
	"context"
	"errors"
	"math"
	"math/big"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/admin"
)

type mcnSettlementItem struct {
	source, id, group                   string
	paidAt                              time.Time
	gross, charge, reported, debtPoints int64
	debtLD                              *int64
	receiptID                           int64
	receiptNo                           string
}

func (s *Store) FinishMCNMembership(ctx context.Context, actor admin.Principal, path string, streamerID int64) error {
	if actor.Area != admin.MCNArea || !actor.Admin || actor.AgencyID <= 0 {
		return admin.ErrBackofficeForbidden
	}
	return s.mcnChange(ctx, func(tx pgx.Tx) error {
		var membership int64
		var status int
		var joined *time.Time
		err := tx.QueryRow(ctx, `SELECT id,status,joined_at FROM backoffice_mcn_streamers
			WHERE mcn_id=$1 AND streamer_id=$2 AND NOT superseded FOR UPDATE`, actor.AgencyID, streamerID).
			Scan(&membership, &status, &joined)
		if errors.Is(err, pgx.ErrNoRows) {
			return admin.ErrBackofficeNotFound
		}
		if err != nil {
			return err
		}
		if joined == nil {
			return admin.ErrMCNReconcile
		}
		if status == admin.MCNNoRenewal {
			var exists bool
			err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM mcn_final_settlements
				WHERE membership_id=$1 AND joined_at=$2 AND agency_id=$3)`, membership, *joined, actor.AgencyID).Scan(&exists)
			if err == nil && !exists {
				return admin.ErrMCNReconcile
			}
			return err
		}
		if status != admin.MCNJoinFinish {
			return admin.ErrMCNState
		}
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout='4s'`); err != nil {
			return err
		}
		// Take a short write barrier across incoming receipts. A debit that
		// started during this contract must be resolved before final settlement.
		if _, err := tx.Exec(ctx, `LOCK TABLE gift_orders,date_gift_orders,room_wheel_orders,anchor_post_orders,
			subscription_orders,mcn_legacy_settlement_proofs,finance_payment_attempts,finance_bad_debt_receipts IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			return err
		}
		var pending bool
		err = tx.QueryRow(ctx, `SELECT
			 EXISTS(SELECT 1 FROM gift_orders WHERE receiver_id=$1 AND state=0)
			 OR EXISTS(SELECT 1 FROM date_gift_orders WHERE anchor_id=$1 AND state=0)
			 OR EXISTS(SELECT 1 FROM room_wheel_orders WHERE anchor_id=$1 AND state=0)
			 OR EXISTS(SELECT 1 FROM anchor_post_orders o JOIN anchor_posts p ON p.id=o.post_id WHERE p.owner_id=$1 AND o.state=0)
			 OR EXISTS(SELECT 1 FROM subscription_orders o JOIN subscription_plans p ON p.id=o.plan_id WHERE p.anchor_id=$1 AND o.status=0)
			 OR EXISTS(SELECT 1 FROM mcn_legacy_settlement_proofs
				WHERE streamer_id=$1 AND agency_id=$2 AND source_pending AND NOT provider_verified AND paid_at >= $3)
			 OR EXISTS(SELECT 1 FROM finance_payment_attempts f WHERE f.action='bad_debt' AND f.state IN ('reserved','unknown') AND f.user_id IN (
				SELECT sender_id FROM gift_orders WHERE receiver_id=$1 AND state=1 AND paid_at>=$3
				UNION ALL SELECT sender_id FROM date_gift_orders WHERE anchor_id=$1 AND state=1 AND paid_at>=$3
				UNION ALL SELECT user_id FROM room_wheel_orders WHERE anchor_id=$1 AND state=1 AND paid_at>=$3
				UNION ALL SELECT o.actor_id FROM anchor_post_orders o JOIN anchor_posts p ON p.id=o.post_id WHERE p.owner_id=$1 AND o.state=1 AND o.paid_at>=$3
				UNION ALL SELECT o.fan_id FROM subscription_orders o JOIN subscription_plans p ON p.id=o.plan_id WHERE p.anchor_id=$1 AND o.status=1 AND o.confirmed_at>=$3
			))`,
			streamerID, actor.AgencyID, *joined).Scan(&pending)
		if err != nil {
			return err
		}
		if pending {
			return admin.ErrMCNReconcile
		}
		var cutoff time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&cutoff); err != nil {
			return err
		}
		items, err := mcnSettlementItems(ctx, tx, streamerID, actor.AgencyID, *joined, cutoff)
		if err != nil {
			return err
		}
		gross, debt, err := mcnSettlementTotals(ctx, tx, items)
		if err != nil {
			return err
		}
		var trace int64
		err = tx.QueryRow(ctx, `INSERT INTO mcn_final_settlements(membership_id,agency_id,streamer_id,joined_at,confirmed_at,gross_ld,bad_debt_ld)
			VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING trace_id`, membership, actor.AgencyID, streamerID, *joined, cutoff, gross, debt).Scan(&trace)
		if err != nil {
			return err
		}
		for _, item := range items {
			_, err = tx.Exec(ctx, `INSERT INTO mcn_final_settlement_items(trace_id,source,source_id,payment_group,provider_order_id,
				provider_order_no,confirmed_points,gross_ld,bad_debt_ld,paid_at)
				VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, trace, item.source, item.id, item.group, item.receiptID, item.receiptNo,
				item.charge, item.gross, *item.debtLD, item.paidAt)
			if err != nil {
				return err
			}
		}
		tag, err := tx.Exec(ctx, `UPDATE anchor_profiles SET agency_id=0 WHERE user_id=$1 AND agency_id=$2`, streamerID, actor.AgencyID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return admin.ErrMCNState
		}
		if _, err := tx.Exec(ctx, `UPDATE backoffice_mcn_streamers SET status=$2 WHERE id=$1`, membership, admin.MCNNoRenewal); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO backoffice_logs(area,tenant,operator_id,operator_name,path,target,action,reason)
			VALUES($1,$2,$3,$4,$5,$6,'mcn_no_renewal',$7)`, actor.Area, actor.Scope(), actor.ID, actor.Name(), path,
			strconv.FormatInt(membership, 10), strconv.FormatInt(trace, 10))
		return err
	})
}

const mcnEntrySQL = `WITH candidates AS (
	SELECT 'gift' AS source,o.id::text AS source_id,'gift:'||o.id AS payment_group,o.paid_at,
		o.income AS gross,o.cost AS charge,COALESCE((o.receipt->>'total_point')::bigint,0) AS reported,
		COALESCE((o.receipt->>'total_bad_debt_point')::bigint,0) AS debt_points,NULL::bigint AS debt_ld,
		CASE WHEN o.receipt->>'order_id'=o.provider_order_id::text THEN o.provider_order_id ELSE 0 END AS proof_id,
		o.receipt->>'order_no' AS proof_no
	FROM gift_orders o WHERE o.receiver_id=$1 AND o.state=1 AND o.cost>0 AND COALESCE(o.item->>'effectControlType','0')<>'7'
		AND o.paid_at >= $3 AND o.paid_at < $4
	UNION ALL
	SELECT 'rocket_refuel_award',a.id::text,'gift:'||o.id,a.settled_at,a.income,o.cost,COALESCE((o.receipt->>'total_point')::bigint,0),
		COALESCE((o.receipt->>'total_bad_debt_point')::bigint,0),NULL::bigint,
		CASE WHEN o.receipt->>'order_id'=o.provider_order_id::text THEN o.provider_order_id ELSE 0 END,o.receipt->>'order_no'
	FROM rocket_refuel_awards a JOIN gift_orders o ON o.id=a.order_id WHERE a.anchor_id=$1 AND o.state=1
		AND a.settled_at >= $3 AND a.settled_at < $4
	UNION ALL
	SELECT 'date_gift',d.id::text,'date_gift:'||d.id,d.paid_at,(d.order_data->>'Income')::bigint,(d.order_data->>'Cost')::bigint,
		COALESCE((d.order_data#>>'{Receipt,total_point}')::bigint,0),COALESCE((d.order_data#>>'{Receipt,total_bad_debt_point}')::bigint,0),NULL::bigint,
		CASE WHEN d.order_data#>>'{Receipt,order_id}'=d.provider_id::text THEN d.provider_id ELSE 0 END,d.order_data#>>'{Receipt,order_no}'
	FROM date_gift_orders d WHERE d.anchor_id=$1 AND d.state=1 AND (d.order_data->>'Cost')::bigint>0
		AND d.paid_at >= $3 AND d.paid_at < $4
	UNION ALL
	SELECT 'wheel',w.id::text,'wheel:'||w.id,w.paid_at,(w.order_data->>'Price')::bigint,(w.order_data->>'Price')::bigint,
		COALESCE((w.order_data#>>'{Receipt,total_point}')::bigint,0),COALESCE((w.order_data#>>'{Receipt,total_bad_debt_point}')::bigint,0),NULL::bigint,
		CASE WHEN w.order_data#>>'{Receipt,order_id}'=w.provider_id::text THEN w.provider_id ELSE 0 END,w.order_data#>>'{Receipt,order_no}'
	FROM room_wheel_orders w WHERE w.anchor_id=$1 AND w.state=1 AND w.paid_at >= $3 AND w.paid_at < $4
	UNION ALL
	SELECT 'post',o.order_no,'post:'||o.order_no,o.paid_at,o.points,o.points,o.points,
		COALESCE(o.provider_bad_debt,0),NULL::bigint,o.provider_id,o.provider_no
	FROM anchor_post_orders o JOIN anchor_posts p ON p.id=o.post_id WHERE p.owner_id=$1 AND o.state=1
		AND o.paid_at >= $3 AND o.paid_at < $4
	UNION ALL
	SELECT 'subscription',o.order_no,'subscription:'||o.order_no,o.confirmed_at,o.points,o.points,COALESCE(o.provider_points,0),
		COALESCE(o.provider_bad_debt,0),NULL::bigint,o.provider_order_id,o.provider_order_no
	FROM subscription_orders o JOIN subscription_plans p ON p.id=o.plan_id WHERE p.anchor_id=$1 AND o.status=1
		AND o.confirmed_at >= $3 AND o.confirmed_at < $4
	UNION ALL
	SELECT 'legacy',l.settlement_id::text,'legacy:'||l.provider_order_id,l.paid_at,l.gross_ld,
		l.charged,l.charged,0,l.bad_debt_ld,l.provider_order_id,l.provider_order_no
	FROM mcn_legacy_settlement_proofs l WHERE l.streamer_id=$1 AND l.agency_id=$2 AND l.source_pending AND l.provider_verified
		AND l.paid_at >= $3 AND l.paid_at < $4
)
SELECT source,source_id,payment_group,paid_at,gross,charge,reported,COALESCE(d.bad_debt_points,c.debt_points),
	CASE WHEN c.debt_ld IS NOT NULL AND d.provider_order_id IS NOT NULL THEN trunc(c.gross::numeric*d.bad_debt_points/d.charged_points)::bigint ELSE c.debt_ld END,
	COALESCE(proof_id,0),COALESCE(proof_no,'')
	FROM candidates c LEFT JOIN finance_bad_debt_receipts d ON d.provider_order_id=c.proof_id AND d.provider_order_no=c.proof_no AND d.charged_points=c.charge
	WHERE NOT EXISTS(SELECT 1 FROM mcn_final_settlement_items i WHERE i.source=c.source AND i.source_id=c.source_id)
	ORDER BY paid_at,source,source_id`

func mcnSettlementItems(ctx context.Context, tx pgx.Tx, streamer, agency int64, joined, cutoff time.Time) ([]mcnSettlementItem, error) {
	rows, err := tx.Query(ctx, mcnEntrySQL, streamer, agency, joined, cutoff)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (mcnSettlementItem, error) {
		var item mcnSettlementItem
		err := row.Scan(&item.source, &item.id, &item.group, &item.paidAt, &item.gross, &item.charge, &item.reported, &item.debtPoints,
			&item.debtLD, &item.receiptID, &item.receiptNo)
		return item, err
	})
}

type mcnPaymentGroup struct {
	key, number string
	total       int64
}

func mcnSettlementTotals(ctx context.Context, tx pgx.Tx, items []mcnSettlementItem) (gross, debt int64, err error) {
	groups := make(map[int64]mcnPaymentGroup, len(items))
	numbers := make(map[string]int64, len(items))
	ids := make([]int64, 0, len(items))
	for i := range items {
		item := &items[i]
		if item.receiptID <= 0 || item.receiptNo == "" || item.charge <= 0 || item.reported != item.charge || item.gross < 0 ||
			item.debtPoints < 0 || item.debtPoints > item.charge {
			return 0, 0, admin.ErrMCNReconcile
		}
		group := mcnPaymentGroup{item.group, item.receiptNo, item.charge}
		if old, exists := groups[item.receiptID]; exists && old != group {
			return 0, 0, admin.ErrMCNReconcile
		}
		if id, exists := numbers[item.receiptNo]; exists && id != item.receiptID {
			return 0, 0, admin.ErrMCNReconcile
		}
		if _, exists := groups[item.receiptID]; !exists {
			ids = append(ids, item.receiptID)
		}
		groups[item.receiptID], numbers[item.receiptNo] = group, item.receiptID
		if item.debtLD == nil {
			bad := new(big.Int).Mul(big.NewInt(item.gross), big.NewInt(item.debtPoints))
			value := bad.Quo(bad, big.NewInt(item.charge)).Int64()
			item.debtLD = &value
		}
		if *item.debtLD < 0 || *item.debtLD > item.gross || gross > math.MaxInt64-item.gross {
			return 0, 0, admin.ErrMCNReconcile
		}
		gross, debt = gross+item.gross, debt+*item.debtLD
	}
	if len(ids) == 0 {
		return gross, debt, nil
	}
	receipts := make([]string, 0, len(numbers))
	for receipt := range numbers {
		receipts = append(receipts, receipt)
	}
	old, err := tx.Query(ctx, `SELECT DISTINCT provider_order_id,provider_order_no,payment_group,confirmed_points
		FROM mcn_final_settlement_items WHERE provider_order_id=ANY($1) OR provider_order_no=ANY($2)`, ids, receipts)
	if err != nil {
		return 0, 0, err
	}
	defer old.Close()
	for old.Next() {
		var id int64
		var group mcnPaymentGroup
		if err := old.Scan(&id, &group.number, &group.key, &group.total); err != nil {
			return 0, 0, err
		}
		if current, exists := groups[id]; !exists || current != group {
			return 0, 0, admin.ErrMCNReconcile
		}
	}
	return gross, debt, old.Err()
}
