package postgres

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/posts"
)

func bindStaticReviews(ctx context.Context, tx pgx.Tx, owner, object int64, fields ...int) error {
	for _, review := range posts.StaticReviews(ctx) {
		if !slices.Contains(fields, review.Kind) {
			continue
		}
		var version int64
		err := tx.QueryRow(ctx, `SELECT version FROM static_review_versions WHERE kind=$1 AND owner_id=$2 AND object_id=$3 AND slot=$4`,
			review.Kind, owner, object, review.Slot).Scan(&version)
		if errors.Is(err, pgx.ErrNoRows) {
			err = tx.QueryRow(ctx, `INSERT INTO static_review_versions(kind,owner_id,object_id,slot) VALUES($1,$2,$3,$4) RETURNING version`,
				review.Kind, owner, object, review.Slot).Scan(&version)
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO static_review_requests(request_id,kind,owner_id,object_id,callback_object,slot,content,version)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(request_id) DO NOTHING`, review.RequestID, review.Kind, owner,
			object, review.CallbackObject, review.Slot, review.Content, version); err != nil {
			return err
		}
	}
	return nil
}

type staticReviewField struct {
	lock, clear string
	noticeKey   string
}

var staticReviewFields = map[int]staticReviewField{
	posts.ReviewPlanName: {
		`SELECT name FROM subscription_plans WHERE id=$1 AND anchor_id=$2 AND NOT deleted FOR UPDATE`,
		`UPDATE subscription_plans SET name='',updated_at=statement_timestamp() WHERE id=$1`, "chat_guard"},
	posts.ReviewOfflineInfo: {
		`SELECT offline_info FROM rooms WHERE id=$1 AND owner_id=$2 FOR UPDATE`,
		`UPDATE rooms SET offline_info='',updated_at=statement_timestamp() WHERE id=$1`, ""},
	posts.ReviewWelcome: {
		`SELECT welcome FROM message_config WHERE user_id=$1 AND user_id=$2 FOR UPDATE`,
		`UPDATE message_config SET welcome='' WHERE user_id=$1`, "chat-sub-welcome"},
	posts.ReviewRoomTitle: {
		`SELECT title FROM rooms WHERE id=$1 AND owner_id=$2 FOR UPDATE`,
		`UPDATE rooms SET title='',updated_at=statement_timestamp() WHERE id=$1`, "BroadcastRoom_default_name"},
	posts.ReviewRoomBanner: {
		`SELECT banner FROM rooms WHERE id=$1 AND owner_id=$2 FOR UPDATE`,
		`UPDATE rooms SET banner=COALESCE((SELECT profile->>'Photo' FROM users WHERE id=rooms.owner_id),''),updated_at=statement_timestamp() WHERE id=$1`, ""},
	posts.ReviewWheelOption: {
		`SELECT COALESCE(options[$3],'') FROM room_wheel_configs WHERE anchor_id=$1 AND anchor_id=$2 FOR UPDATE`,
		`UPDATE room_wheel_configs SET options[$2]='******' WHERE anchor_id=$1`, ""},
	posts.ReviewProfileBanner: {
		`SELECT COALESCE(profile->>'Banner','') FROM users WHERE id=$1 AND id=$2 FOR UPDATE`,
		`UPDATE users SET profile=jsonb_set(profile,'{Banner}','""') WHERE id=$1`, ""},
	posts.ReviewIntro: {
		`SELECT COALESCE(details->>'Intro','') FROM anchor_profiles WHERE user_id=$1 AND user_id=$2 FOR UPDATE`,
		`UPDATE anchor_profiles SET details=jsonb_set(details,'{Intro}','""') WHERE user_id=$1`, ""},
	posts.ReviewAvatar: {
		`SELECT COALESCE(profile->>'Photo','') FROM users WHERE id=$1 AND id=$2 FOR UPDATE`,
		`UPDATE users SET profile=jsonb_set(profile,'{Photo}','""') WHERE id=$1`, ""},
	posts.ReviewDisplayName: {
		`SELECT account FROM users WHERE id=$1 AND id=$2 FOR UPDATE`,
		`UPDATE users SET account=$2 WHERE id=$1`, ""},
	posts.ReviewSearchName: {
		`SELECT COALESCE(profile->>'SearchName',account) FROM users WHERE id=$1 AND id=$2 FOR UPDATE`,
		`UPDATE users SET profile=jsonb_set(profile,'{SearchName}',to_jsonb($2::text)) WHERE id=$1`, ""},
}

func (s *Store) ApplyStaticReview(ctx context.Context, request string, owner, callbackObject int64, kind, operation int, content string) error {
	field, supported := staticReviewFields[kind]
	if !supported {
		return posts.ErrNotFound
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var object, version int64
		var slot int
		var previous *int
		err := tx.QueryRow(ctx, `SELECT object_id,slot,version,operation FROM static_review_requests
			WHERE request_id=$1 AND kind=$2 AND owner_id=$3 AND callback_object=$4 AND content=$5 FOR UPDATE`,
			request, kind, owner, callbackObject, content).Scan(&object, &slot, &version, &previous)
		if errors.Is(err, pgx.ErrNoRows) {
			return posts.ErrNotFound
		}
		if err != nil || previous != nil {
			return err
		}
		applied := false
		if operation != 1 {
			applied, err = applyStaticRejection(ctx, tx, field, kind, owner, object, slot, version, content)
			if err != nil {
				return err
			}
		}
		if applied {
			target := 1
			if kind >= posts.ReviewAvatar {
				target = 3
			}
			if _, err := tx.Exec(ctx, `INSERT INTO notices(id,notice_target,notice_type,receive_id,content,create_time)
				VALUES(nextval('local_notice_ids'),$1,$2,$3,jsonb_build_object('key',$4::text,'content',''),now())`,
				target, 18+kind, owner, field.noticeKey); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `UPDATE static_review_requests SET operation=$2,applied=$3,decided_at=statement_timestamp() WHERE request_id=$1`, request, operation, applied)
		return err
	})
}

func applyStaticRejection(ctx context.Context, tx pgx.Tx, field staticReviewField, kind int, owner, object int64, slot int, version int64, content string) (bool, error) {
	if kind == posts.ReviewSearchName {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(167884962,44)`); err != nil {
			return false, err
		}
	}
	var current string
	args := []any{object, owner}
	if kind == posts.ReviewWheelOption {
		args = append(args, slot)
	}
	err := tx.QueryRow(ctx, field.lock, args...).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && current != content {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var stillCurrent bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM static_review_versions
		WHERE kind=$1 AND owner_id=$2 AND object_id=$3 AND slot=$4 AND version=$5)`, kind, owner, object, slot, version).Scan(&stillCurrent); err != nil || !stillCurrent {
		return false, err
	}
	args = []any{object}
	switch kind {
	case posts.ReviewWheelOption:
		args = append(args, slot)
	case posts.ReviewDisplayName:
		args = append(args, fmt.Sprintf("User%d", 1_000_000+rand.IntN(9_000_000)))
	case posts.ReviewSearchName:
		name, err := staticReplacementSearchName(ctx, tx, owner)
		if err != nil {
			return false, err
		}
		args = append(args, name)
	}
	if _, err := tx.Exec(ctx, field.clear, args...); err != nil {
		return false, err
	}
	if kind == posts.ReviewWelcome {
		if _, err := tx.Exec(ctx, `UPDATE anchor_profiles SET details=jsonb_set(details,'{Welcome}','""') WHERE user_id=$1`, owner); err != nil {
			return false, err
		}
	}
	return true, nil
}

func staticReplacementSearchName(ctx context.Context, tx pgx.Tx, owner int64) (string, error) {
	for range 10 {
		name := fmt.Sprintf("t%08d", 1+rand.IntN(99_999_999))
		var available bool
		if err := tx.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM users WHERE id<>$1 AND lower(COALESCE(profile->>'SearchName',account))=$2)`,
			owner, name).Scan(&available); err != nil {
			return "", err
		}
		if available {
			return name, nil
		}
	}
	return "", errors.New("could not allocate a replacement search name")
}
