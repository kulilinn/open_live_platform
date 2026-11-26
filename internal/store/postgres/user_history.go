package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/admin"
)

func (s *Store) BackofficeChargeHistory(ctx context.Context, q admin.UserHistoryQuery) ([]admin.ChargeHistoryRow, int64, error) {
	const relation = `FROM legacy_business_history h CROSS JOIN LATERAL jsonb_to_record(h.record) r
		(amount numeric,point bigint,payment_method_name text,status smallint,create_time timestamp)
		CROSS JOIN LATERAL (SELECT r.create_time AT TIME ZONE 'UTC' AS created) ts
		WHERE h.source_table='pay_orders' AND h.owner_id=$1 AND ($2::smallint IS NULL OR r.status=$2)
		AND ($3::timestamptz IS NULL OR ts.created >= $3) AND ($4::timestamptz IS NULL OR ts.created <= $4)`
	const columns = `h.source_id::bigint,r.amount::text,r.point,r.payment_method_name,r.status,ts.created`
	return readBackofficePage(ctx, s, q.PageRequest, columns, relation, "ts.created DESC,h.source_id::bigint DESC",
		[]any{q.UID, q.Status, q.Start, q.End}, func(row pgx.CollectableRow) (admin.ChargeHistoryRow, error) {
			var item admin.ChargeHistoryRow
			err := row.Scan(&item.ID, &item.Price, &item.Points, &item.Method, &item.Status, &item.Created)
			item.Created = item.Created.UTC()
			return item, err
		})
}

const consumeRelation = `FROM (
	SELECT o.provider_order_id AS id,o.receipt->>'order_no' AS receipt,2::smallint AS ty,o.cost AS points,o.item->>'name' AS name,o.paid_at AS ts,TRUE AS native
	FROM gift_orders o WHERE o.sender_id=$1 AND o.state=1 AND o.cost>0
	UNION ALL SELECT o.provider_id,o.order_data#>>'{Receipt,order_no}',2,(o.order_data->>'Cost')::bigint,o.order_data#>>'{Item,name}',o.paid_at,TRUE
	FROM date_gift_orders o WHERE o.sender_id=$1 AND o.state=1 AND (o.order_data->>'Cost')::bigint>0
	UNION ALL SELECT w.provider_id,w.order_data#>>'{Receipt,order_no}',11,(w.order_data->>'Price')::bigint,NULL,w.paid_at,TRUE
	FROM room_wheel_orders w WHERE w.user_id=$1 AND w.state=1
	UNION ALL SELECT o.provider_id,o.provider_no,o.kind+3,o.points,NULL,o.paid_at,TRUE
	FROM anchor_post_orders o WHERE o.actor_id=$1 AND o.state=1
	UNION ALL SELECT o.provider_order_id,o.provider_order_no,CASE WHEN o.renewal_from IS NULL THEN 3 ELSE 10 END,o.points,NULL,o.confirmed_at,TRUE
	FROM subscription_orders o WHERE o.fan_id=$1 AND o.status=1
	UNION ALL SELECT h.source_id::bigint,r.no,r.transaction_type,r.total_point,g.record->>'name',r.create_time AT TIME ZONE 'UTC',FALSE
	FROM legacy_business_history h CROSS JOIN LATERAL jsonb_to_record(h.record) r
		(no text,transaction_type smallint,total_point bigint,transaction_order_id bigint,create_time timestamp,income_or_expenses smallint)
	LEFT JOIN legacy_business_history m ON m.source_table='gift_production_marketing' AND m.source_id=r.transaction_order_id::text
	LEFT JOIN legacy_business_history g ON g.source_table='gifts' AND g.source_id=m.record->>'gift_id'
	WHERE h.source_table='point_transactions' AND h.owner_id=$1 AND r.income_or_expenses=-1
) entries WHERE ($2::smallint IS NULL OR ty=$2) AND ($3::timestamptz IS NULL OR ts>=$3) AND ($4::timestamptz IS NULL OR ts<=$4)
	AND (native OR receipt IS NULL OR NOT EXISTS(
		SELECT 1 FROM gift_orders o WHERE o.state=1 AND o.sender_id=$1 AND o.provider_order_id=entries.id AND o.receipt->>'order_no'=entries.receipt
		UNION ALL SELECT 1 FROM date_gift_orders o WHERE o.state=1 AND o.sender_id=$1 AND o.provider_id=entries.id AND o.order_data#>>'{Receipt,order_no}'=entries.receipt
		UNION ALL SELECT 1 FROM room_wheel_orders w WHERE w.state=1 AND w.user_id=$1 AND w.provider_id=entries.id AND w.order_data#>>'{Receipt,order_no}'=entries.receipt
		UNION ALL SELECT 1 FROM anchor_post_orders o WHERE o.state=1 AND o.actor_id=$1 AND o.provider_id=entries.id AND o.provider_no=entries.receipt
		UNION ALL SELECT 1 FROM subscription_orders o WHERE o.status=1 AND o.fan_id=$1 AND o.provider_order_id=entries.id AND o.provider_order_no=entries.receipt
	))`

func (s *Store) BackofficeConsumeHistory(ctx context.Context, q admin.UserHistoryQuery) ([]admin.ConsumeHistoryRow, int64, error) {
	return readBackofficePage(ctx, s, q.PageRequest, "ty,points,name,ts", consumeRelation, "ts DESC,id DESC,native DESC",
		[]any{q.UID, q.Type, q.Start, q.End}, func(row pgx.CollectableRow) (admin.ConsumeHistoryRow, error) {
			var item admin.ConsumeHistoryRow
			err := row.Scan(&item.Type, &item.Points, &item.Name, &item.Created)
			item.Created = item.Created.UTC()
			return item, err
		})
}
