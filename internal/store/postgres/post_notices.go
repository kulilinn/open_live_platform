package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/posts"
	"open_live_platform/internal/subscription"
)

func publishPostNotices(ctx context.Context, tx pgx.Tx, id, owner int64, level int) error {
	if level == 1 {
		return nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO notices(id,notice_target,notice_type,receive_id,sender_avatar,content,biz_id,create_time,source_key)
		SELECT nextval('local_notice_ids'),2,2,fan,COALESCE(u.profile->>'Photo',''),
			jsonb_build_object('anchor_name',u.account),$1,statement_timestamp(),'post:published:' || $1::bigint::text
		FROM users u CROSS JOIN (
			SELECT DISTINCT m.fan_id AS fan
			FROM subscription_members m JOIN subscription_plans p ON p.id=m.plan_id
			WHERE m.anchor_id=$2 AND m.cancelled_at IS NULL AND m.expires_at>now()
			AND NOT EXISTS (SELECT 1 FROM subscription_saved_users blocked
				WHERE blocked.owner_id=m.fan_id AND blocked.person_id=$2 AND blocked.kind=3)
			AND ($3=0 OR COALESCE(m.legacy_level,p.plan_type-2) BETWEEN $3-2 AND 2)
		) fans WHERE u.id=$2
		ON CONFLICT (receive_id,source_key) WHERE source_key IS NOT NULL DO NOTHING`, id, owner, level)
	return err
}

func addPostInteractionNotice(ctx context.Context, tx pgx.Tx, typ int, post, actor, points int64, key string) error {
	_, err := tx.Exec(ctx, `INSERT INTO notices(id,notice_target,notice_type,receive_id,sender_avatar,content,biz_id,create_time,source_key)
		SELECT nextval('local_notice_ids'),2,$3,p.owner_id,COALESCE(u.profile->>'Photo',''),
			CASE WHEN $4::bigint>0 THEN jsonb_build_object('user_name',u.account,'amount',$4)
			ELSE jsonb_build_object('user_name',u.account) END,$1,statement_timestamp(),$5
		FROM anchor_posts p JOIN users u ON u.id=$2
		WHERE p.id=$1 AND p.owner_id<>$2
		AND NOT EXISTS (SELECT 1 FROM subscription_saved_users blocked
			WHERE blocked.owner_id=p.owner_id AND blocked.person_id=$2 AND blocked.kind=$6)
		ON CONFLICT (receive_id,source_key) WHERE source_key IS NOT NULL DO NOTHING`,
		post, actor, typ, points, key, subscription.Blacklist)
	return err
}

func addPostLikeNotice(ctx context.Context, tx pgx.Tx, post, fan int64) error {
	return addPostInteractionNotice(ctx, tx, posts.NoticeLike, post, fan, 0, fmt.Sprintf("post:like:%d:%d", post, fan))
}

func addPostCommentNotice(ctx context.Context, tx pgx.Tx, post, fan, comment int64) error {
	return addPostInteractionNotice(ctx, tx, posts.NoticeComment, post, fan, 0, fmt.Sprintf("post:comment:%d", comment))
}

func addPostRejectionNotice(ctx context.Context, tx pgx.Tx, typ int8, receiver int64, key string) error {
	if typ == 0 {
		return nil
	}
	target := 1
	if typ == posts.NoticeCommentRejection {
		target = 3
	}
	_, err := tx.Exec(ctx, `INSERT INTO notices(id,notice_target,notice_type,receive_id,content,create_time,source_key)
		VALUES(nextval('local_notice_ids'),$1,$2,$3,'{"key":"","content":""}',statement_timestamp(),$4)
		ON CONFLICT (receive_id,source_key) WHERE source_key IS NOT NULL DO NOTHING`, target, typ, receiver, key)
	return err
}
