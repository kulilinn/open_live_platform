package postgres

import (
	"context"
	"time"
)

// SendSubscriptionReminders delivers the original seven-day fan and anchor notices.
// The expiration is part of the key, so renewal can schedule another pair.
func (s *Store) SendSubscriptionReminders(ctx context.Context, now time.Time) (int64, error) {
	result, err := s.pool.Exec(ctx, `WITH due AS (
  SELECT m.fan_id,m.anchor_id,p.name,m.expires_at,t.anchor,t.recipient,t.sender,
   concat('sub:expiry:',m.fan_id,':',m.plan_id,':',extract(epoch FROM m.expires_at),':',t.anchor) source
  FROM subscription_members m JOIN subscription_plans p ON p.id=m.plan_id
  CROSS JOIN LATERAL (VALUES(true,m.anchor_id,m.fan_id),(false,m.fan_id,m.anchor_id)) t(anchor,recipient,sender)
  WHERE m.expires_at>$1 AND m.expires_at<=$1::timestamptz+interval '7 days'
   AND m.created_at<=m.expires_at-interval '7 days' AND m.cancelled_at IS NULL
 )
 INSERT INTO notices(id,notice_target,notice_type,receive_id,sender_avatar,content,create_time,source_key)
 SELECT nextval('local_notice_ids'),CASE WHEN d.anchor THEN 1 ELSE 2 END,CASE WHEN d.anchor THEN 3 ELSE 6 END,d.recipient,
  coalesce(u.profile->>'Photo',''),jsonb_build_object('search_name',coalesce(u.profile->>'SearchName',u.account),
    'anchor_id',d.anchor_id::text,'fans_id',d.fan_id::text,'user_name',u.account,'is_anchor',d.anchor,
    'plan_name',d.name,'expiration_date',$1::timestamptz),$1,d.source
 FROM due d JOIN users u ON u.id=d.sender
 WHERE NOT EXISTS(SELECT 1 FROM notices n WHERE n.receive_id=d.recipient AND n.source_key=d.source)
 ORDER BY d.recipient,d.source ON CONFLICT(receive_id,source_key) WHERE source_key IS NOT NULL DO NOTHING`, now)
	return result.RowsAffected(), err
}
