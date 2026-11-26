package postgres

import (
	"context"
	"crypto/rand"
	"errors"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/admin"
)

func queueRoomModerationMail(ctx context.Context, tx pgx.Tx, op admin.Action) error {
	if op.Kind != "streamer_disable" && op.Kind != "streamer_enable" {
		return nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO room_moderation_mail(room_id,user_id,action,email,display_name,locale,ban_until,reasons)
		SELECT r.id,u.id,$3,u.email,u.account,
			CASE u.language WHEN 'zh-cn' THEN 'zh_cn' WHEN 'zh-Hant' THEN 'zh_tw' WHEN 'zh_Hant' THEN 'zh_tw' WHEN 'en' THEN 'en_us' ELSE 'ja_jp' END,
			r.moderation_until,COALESCE($4::text[],'{}')
		FROM rooms r JOIN users u ON u.id=r.owner_id WHERE r.id=$1 AND u.id=$2 AND COALESCE(u.email,'')<>''`,
		op.RoomID, op.UserID, op.Kind, op.Reason)
	return err
}

func (s *Store) NextRoomModerationMail(ctx context.Context) (admin.RoomModerationMail, bool, error) {
	mail := admin.RoomModerationMail{Lease: rand.Text()}
	err := s.pool.QueryRow(ctx, `WITH next AS (
		SELECT q.id,COALESCE(q.template,provider.texts->>q.locale) AS template,
			COALESCE(q.fields,jsonb_build_object('anchor_name',q.display_name,'anchor_address',q.email) ||
			CASE q.action WHEN 'streamer_enable' THEN jsonb_build_object('live_id',q.room_id::text)
			ELSE jsonb_build_object('ban_time',CASE WHEN q.ban_until=-1 THEN permanent.texts->>q.locale
				WHEN q.ban_until>0 THEN to_char(to_timestamp(q.ban_until) AT TIME ZONE 'Asia/Tokyo','MM/DD/YYYY HH24:MI') ELSE '' END) ||
				COALESCE(reasons.localized,'{}'::jsonb) END) AS fields
		FROM room_moderation_mail q
		LEFT JOIN room_moderation_mail_templates provider ON provider.key=CASE q.action
			WHEN 'streamer_disable' THEN 'email_room_banned_template' ELSE 'email_room_banned_renew_template' END
		LEFT JOIN room_moderation_mail_templates permanent ON permanent.key='email_room_banned_time_template'
		LEFT JOIN LATERAL (
			SELECT jsonb_object_agg('reason_'||r.number,COALESCE(t.texts->>q.locale,'')) AS localized
			FROM unnest(q.reasons) WITH ORDINALITY r(key,number)
			LEFT JOIN room_moderation_mail_templates t ON t.key=r.key
		) reasons ON TRUE
		WHERE q.sent_at IS NULL AND q.attempts<5 AND q.next_attempt<=statement_timestamp()
			AND (q.lease_until IS NULL OR q.lease_until<statement_timestamp())
			AND COALESCE(q.template,provider.texts->>q.locale,'')<>''
			AND (q.fields IS NOT NULL OR q.action='streamer_enable' OR q.ban_until<>-1 OR COALESCE(permanent.texts->>q.locale,'')<>'')
			AND NOT EXISTS (SELECT 1 FROM room_moderation_mail p WHERE p.room_id=q.room_id AND p.id<q.id
				AND p.sent_at IS NULL AND (p.attempts<5 OR p.lease_until>statement_timestamp()))
		ORDER BY q.next_attempt,q.id FOR UPDATE OF q SKIP LOCKED LIMIT 1
	)
	UPDATE room_moderation_mail q SET template=next.template,fields=next.fields,lease_id=$1,
		lease_until=statement_timestamp()+interval '30 seconds',attempts=q.attempts+1
	FROM next WHERE q.id=next.id RETURNING q.id,q.delivery_key::text,q.template,q.email,q.display_name,q.fields`, mail.Lease).
		Scan(&mail.ID, &mail.DeliveryKey, &mail.Template, &mail.Recipient.Address, &mail.Recipient.Name, &mail.Fields)
	if errors.Is(err, pgx.ErrNoRows) {
		return admin.RoomModerationMail{}, false, nil
	}
	return mail, err == nil, err
}

func (s *Store) CompleteRoomModerationMail(ctx context.Context, id int64, lease string, success bool) error {
	_, err := s.pool.Exec(ctx, `UPDATE room_moderation_mail SET
		sent_at=CASE WHEN $3 THEN statement_timestamp() ELSE NULL END,
		next_attempt=statement_timestamp()+(1 << attempts)*interval '1 minute',lease_id=NULL,lease_until=NULL
		WHERE id=$1 AND lease_id=$2`, id, lease, success)
	return err
}
