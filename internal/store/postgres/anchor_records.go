package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/admin"
	"open_live_platform/internal/auth"
)

const identityColumns = `k.submitted || jsonb_build_object('user_id',k.user_id,'applicant_id',COALESCE(k.applicant_id,''),
	'status',k.status,'create_time',to_char(k.created_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),'_version',NULL)`

func (s *Store) AnchorIdentityPage(ctx context.Context, page admin.PageRequest, name *string) ([]json.RawMessage, int64, error) {
	return readBackofficePage(ctx, s, page, identityColumns, "FROM anchor_kyc_applicants k WHERE ($1::text IS NULL OR k.submitted->>'name'=$1)", "k.created_at DESC,k.user_id DESC", []any{name}, pgx.RowTo[json.RawMessage])
}

func (s *Store) AnchorReviewDetail(ctx context.Context, id int64, identity bool) (json.RawMessage, error) {
	query := `SELECT jsonb_build_object('id',a.user_id::text,'displayName',u.account,'mail',COALESCE(u.email,u.legacy_profile_email,''),
		'name',k.submitted->'name','birth',k.submitted->'birth','gender',k.submitted->'gender','country',k.submitted->'country',
		'area',k.submitted->'area','code',k.submitted->'code','address',k.submitted->'address','idCard',k.submitted->'id_card',
		'phoneNumber',k.submitted->'phone_number','extra',k.submitted->'extra','kycStatus',a.kyc_status,'rejectReason',NULLIF(a.reason,''))
		FROM anchor_profiles a JOIN users u ON u.id=a.user_id LEFT JOIN anchor_kyc_applicants k ON k.user_id=a.user_id WHERE a.user_id=$1`
	if identity {
		query = "SELECT " + identityColumns + " FROM anchor_kyc_applicants k WHERE k.user_id=$1"
	}
	var data json.RawMessage
	err := s.pool.QueryRow(ctx, query, id).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		err = admin.ErrBackofficeNotFound
	}
	return data, err
}

func (s *Store) CancelAnchorReview(ctx context.Context, actor admin.Principal, path string, id int64) error {
	return s.change(ctx, id, func(r *roomChange) error {
		var u auth.User
		u.ID = id
		if err := r.tx.QueryRow(ctx, `SELECT u.account,u.profile FROM users u JOIN anchor_profiles a ON a.user_id=u.id WHERE u.id=$1 FOR UPDATE OF u,a`, id).Scan(&u.Account, &u.Profile); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return admin.ErrBackofficeNotFound
			}
			return err
		}
		var online bool
		if err := r.tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM rooms WHERE id=$1 AND online)`, id).Scan(&online); err != nil {
			return err
		}
		if online {
			if err := r.close(id); err != nil {
				return err
			}
		}
		profile := auth.PublicProfile(u)
		profile.IsAnchor, profile.GroupRoom = false, ""
		if _, err := r.tx.Exec(ctx, `UPDATE users SET profile=$2 WHERE id=$1`, id, profile); err != nil {
			return err
		}
		for _, q := range []string{
			"INSERT INTO anchor_review_cancellations(user_id) VALUES($1) ON CONFLICT DO NOTHING",
			"DELETE FROM anchor_kyc_applicants WHERE user_id=$1",
			"DELETE FROM anchor_profiles WHERE user_id=$1",
			"UPDATE subscription_plans SET published=false,deleted=true,updated_at=now() WHERE anchor_id=$1 AND NOT deleted",
			"UPDATE subscription_members SET cancelled_at=now() WHERE anchor_id=$1 AND cancelled_at IS NULL",
			"UPDATE dm_conversations SET can_message=false,subscription_expires_at=NULL WHERE anchor_id=$1",
			"UPDATE anchor_posts SET deleted=true WHERE owner_id=$1 AND NOT deleted",
			"UPDATE public_shares SET status=0,updated_at=now() WHERE user_id=$1 AND status>0",
			"UPDATE date_sessions SET ended_at=now() WHERE anchor_id=$1 AND ended_at IS NULL",
			"DELETE FROM date_hosts WHERE anchor_id=$1",
			"DELETE FROM room_wheel_configs WHERE anchor_id=$1",
			"DELETE FROM subscription_groups WHERE owner_id=$1",
		} {
			if _, err := r.tx.Exec(ctx, q, id); err != nil {
				return err
			}
		}
		_, err := r.tx.Exec(ctx, `INSERT INTO backoffice_logs(area,tenant,operator_id,operator_name,path,target,action) VALUES($1,$2,$3,$4,$5,$6,'anchor_cancelled')`, actor.Area, actor.Scope(), actor.ID, actor.Name(), path, strconv.FormatInt(id, 10))
		return err
	})
}

func restoreAnchorReview(ctx context.Context, tx pgx.Tx, row HistoryRow) (int64, error) {
	var q string
	if row.Source == "anchor_users_kyc" {
		q = `INSERT INTO anchor_kyc_applicants(user_id,applicant_id,submitted,status,created_at)
		SELECT u.id,NULLIF(r->>'applicant_id',''),r-ARRAY['applicant_id','user_id','status','create_time'],COALESCE((r->>'status')::smallint,0),
		COALESCE((r->>'create_time')::timestamptz,now()) FROM users u CROSS JOIN (SELECT $2::jsonb r) data
		WHERE u.id=$1 AND NOT EXISTS(SELECT 1 FROM anchor_review_cancellations c WHERE c.user_id=u.id)
		ON CONFLICT(user_id) DO NOTHING`
	} else {
		q = `INSERT INTO anchor_profiles(user_id,state,reason,channel,kyc_status,created_at,details)
		SELECT u.id,(r->>'state')::smallint,COALESCE(r->>'reject_reason',''),COALESCE(r->>'channel',''),COALESCE((r->>'kyc_status')::smallint,0),
		COALESCE((r->>'create_time')::timestamptz,now()),jsonb_strip_nulls(jsonb_build_object('FamilyName',r->'family_name','Country',r->'country','Address',r->'address',
		'Intro',r->'intro','Welcome',r->'welcome','FreeSub',r->'free_sub','twitter_link',r->'twitter','instagram_link',r->'instagram_link','strip_chat_link',r->'strip_chat_link',
		'tiktok_link',r->'tiktok_link','yutube_link',r->'yutube_link'))
		FROM users u CROSS JOIN (SELECT $2::jsonb r) data WHERE u.id=$1 AND (r->>'state')::int IN (0,1,2) AND COALESCE((r->>'del_flag')::int,0)=0
		AND NOT EXISTS(SELECT 1 FROM anchor_review_cancellations c WHERE c.user_id=u.id) ON CONFLICT(user_id) DO NOTHING`
	}
	tag, err := tx.Exec(ctx, q, row.Owner, row.Record)
	return tag.RowsAffected(), err
}
