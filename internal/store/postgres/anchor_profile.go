package postgres

import (
	"context"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/admin"
	"open_live_platform/internal/anchor"
	"open_live_platform/internal/auth"
)

var _ anchor.Store = (*Store)(nil)

const anchorRecord = `SELECT user_id,state,reason,channel,kyc_status,task_started_at,created_at,details
	FROM anchor_profiles WHERE user_id=$1`

func readAnchor(row pgx.Row) (anchor.Record, error) {
	var values, details anchor.Record
	err := row.Scan(&values.ID, &values.State, &values.Reason, &values.Channel, &values.KYCStatus,
		&values.StartedAt, &values.CreatedAt, &details)
	if errors.Is(err, pgx.ErrNoRows) {
		return anchor.Record{}, anchor.ErrNotFound
	}
	details.ID, details.State, details.Reason, details.Channel = values.ID, values.State, values.Reason, values.Channel
	details.KYCStatus, details.StartedAt, details.CreatedAt = values.KYCStatus, values.StartedAt, values.CreatedAt
	if details.FreeSub == 0 {
		details.FreeSub = 1
	}
	return details, err
}

func (s *Store) AnchorApplicant(ctx context.Context, id int64) (string, error) {
	var applicant string
	err := s.pool.QueryRow(ctx, `SELECT applicant_id FROM anchor_kyc_applicants WHERE user_id=$1`, id).Scan(&applicant)
	if errors.Is(err, pgx.ErrNoRows) {
		err = nil
	}
	return applicant, err
}

func (s *Store) SaveAnchorApplicant(ctx context.Context, id int64, applicant string) error {
	tag, err := s.pool.Exec(ctx, `INSERT INTO anchor_kyc_applicants(user_id,applicant_id) VALUES($1,$2)
		ON CONFLICT(user_id) DO UPDATE SET applicant_id=EXCLUDED.applicant_id
		WHERE anchor_kyc_applicants.applicant_id=EXCLUDED.applicant_id`, id, applicant)
	if err == nil && tag.RowsAffected() != 1 {
		err = anchor.ErrKYC
	}
	return err
}

func (s *Store) ConfirmAnchorKYC(ctx context.Context, id int64, applicant string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE anchor_profiles a SET kyc_status=1 FROM anchor_kyc_applicants k
		WHERE a.user_id=$1 AND k.user_id=a.user_id AND k.applicant_id=$2`, id, applicant)
	return err == nil && tag.RowsAffected() == 1, err
}

func (s *Store) Anchor(ctx context.Context, id int64) (anchor.Record, error) {
	record, err := readAnchor(s.pool.QueryRow(ctx, anchorRecord, id))
	if !errors.Is(err, anchor.ErrNotFound) {
		return record, err
	}
	u, _, err := s.Find(ctx, id)
	if err != nil {
		return anchor.Record{}, err
	}
	return anchor.FromIdentity(u)
}

func anchorForUpdate(ctx context.Context, tx pgx.Tx, id int64, allowNew bool) (anchor.Record, auth.User, error) {
	var u auth.User
	u.ID = id
	if err := tx.QueryRow(ctx, `SELECT account,profile FROM users WHERE id=$1 FOR UPDATE`, id).Scan(&u.Account, &u.Profile); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			err = anchor.ErrNotFound
		}
		return anchor.Record{}, u, err
	}
	record, err := readAnchor(tx.QueryRow(ctx, anchorRecord, id))
	if !errors.Is(err, anchor.ErrNotFound) {
		return record, u, err
	}
	record, err = anchor.FromIdentity(u)
	if err != nil && !allowNew {
		return record, u, err
	}
	if errors.Is(err, anchor.ErrNotFound) {
		record = anchor.Record{ID: id, FreeSub: 1}
	}
	_, err = tx.Exec(ctx, `INSERT INTO anchor_profiles(user_id,state) VALUES($1,$2)`, id, record.State)
	return record, u, err
}

func saveAnchorProfile(ctx context.Context, tx pgx.Tx, r anchor.Record) error {
	var details anchor.Record
	details.FamilyName, details.Address, details.Intro = r.FamilyName, r.Address, r.Intro
	details.Welcome, details.Country, details.Label, details.FreeSub = r.Welcome, r.Country, r.Label, r.FreeSub
	details.Social = r.Social
	_, err := tx.Exec(ctx, `UPDATE anchor_profiles SET details=$2 WHERE user_id=$1`, r.ID, details)
	return err
}

func (s *Store) ApplyAnchor(ctx context.Context, user auth.User, apply anchor.Application) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		record, _, err := anchorForUpdate(ctx, tx, user.ID, true)
		if err != nil {
			return err
		}
		record.Apply(apply)
		if err := ensureFreePlan(ctx, tx, user.ID); err != nil {
			return err
		}
		return saveAnchorProfile(ctx, tx, record)
	})
}

func (s *Store) EditAnchor(ctx context.Context, id int64, change anchor.ProfileEdit) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(167884962,44)`); err != nil {
			return err
		}
		record, user, err := anchorForUpdate(ctx, tx, id, false)
		if errors.Is(err, anchor.ErrNotFound) || (err == nil && record.State != anchor.Approved) {
			return anchor.ErrPending
		}
		if err != nil {
			return err
		}
		profile := auth.PublicProfile(user)
		if profile.SearchName != change.SearchName {
			var used bool
			err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id<>$1
				AND lower(COALESCE(profile->>'SearchName',account))=lower($2))`, id, change.SearchName).Scan(&used)
			if err != nil {
				return err
			}
			if used {
				return anchor.ErrNameUsed
			}
		}
		profile.SearchName, profile.Photo, profile.Banner = change.SearchName, change.Image, change.Banner
		_, err = tx.Exec(ctx, `UPDATE users SET account=$2,profile=$3,trans_lang=COALESCE($4,trans_lang),
			trans_enabled=COALESCE($5,trans_enabled) WHERE id=$1`, id, change.DisplayName, profile, change.Language, change.Translate)
		if err != nil {
			return err
		}
		record.Edit(change)
		return saveAnchorProfile(ctx, tx, record)
	})
}

func (s *Store) EditAnchorSocial(ctx context.Context, id int64, social anchor.Social) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		record, _, err := anchorForUpdate(ctx, tx, id, false)
		if errors.Is(err, anchor.ErrNotFound) || (err == nil && record.State != anchor.Approved) {
			return anchor.ErrPending
		}
		if err != nil {
			return err
		}
		record.Social = social
		return saveAnchorProfile(ctx, tx, record)
	})
}

const anchorReviewColumns = `a.user_id,u.account,COALESCE(u.email,u.legacy_profile_email,''),COALESCE(u.profile->>'Photo',''),
	COALESCE(a.details->>'Country',''),a.state,a.kyc_status,` + backofficeAccountStatus + `,
	COALESCE((SELECT teacher_label FROM rooms WHERE id=a.user_id),''),
	a.reason,a.created_at,(SELECT count(DISTINCT fan_id) FROM subscription_members m WHERE m.anchor_id=a.user_id
		AND m.expires_at>statement_timestamp()),COALESCE((SELECT sum(income) FROM gift_orders WHERE receiver_id=a.user_id AND state=1),0)
		+COALESCE((SELECT sum(points) FROM legacy_settlements WHERE anchor_id=a.user_id),0)`

func readAnchorReview(row pgx.Row) (admin.AnchorReviewRow, error) {
	var item admin.AnchorReviewRow
	err := row.Scan(&item.ID, &item.Name, &item.Mail, &item.Thumbnail, &item.Country, &item.State, &item.KYC, &item.AccountStatus,
		&item.TeacherLabel, &item.Reason, &item.CreatedAt, &item.Fans, &item.Earnings)
	return item, err
}

func (s *Store) BackofficeAnchorReviews(ctx context.Context, query admin.AnchorReviewQuery) ([]admin.AnchorReviewRow, int64, error) {
	return readBackofficePage(ctx, s, query.PageRequest, anchorReviewColumns, `FROM anchor_profiles a JOIN users u ON u.id=a.user_id
		WHERE ($1::bigint IS NULL OR a.user_id=$1) AND ($2::text IS NULL OR u.account=$2)
		AND ($3::text IS NULL OR COALESCE(u.email,u.legacy_profile_email)=$3)
		AND ($4::smallint IS NULL OR a.state=$4) AND ($5::smallint IS NULL OR a.kyc_status=$5)
		AND (NULLIF($6::text,'') IS NULL OR a.created_at >= NULLIF($6,'')::date)
		AND (NULLIF($7::text,'') IS NULL OR a.created_at < NULLIF($7,'')::date+1)`,
		"a.created_at DESC,a.user_id DESC", []any{query.ID, query.Name, query.Email, query.State, query.KYC, query.Start, query.End},
		func(row pgx.CollectableRow) (admin.AnchorReviewRow, error) { return readAnchorReview(row) })
}

func (s *Store) ReviewAnchor(ctx context.Context, actor admin.Principal, path string, id int64, state int, reason string, price int64) (admin.AnchorReviewRow, error) {
	var result admin.AnchorReviewRow
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		_, u, err := anchorForUpdate(ctx, tx, id, false)
		if errors.Is(err, anchor.ErrNotFound) {
			return admin.ErrBackofficeNotFound
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE anchor_profiles SET state=$2,reason=$3 WHERE user_id=$1`, id, state, reason); err != nil {
			return err
		}
		profile := auth.PublicProfile(u)
		profile.IsAnchor = state == anchor.Approved
		profile.GroupRoom = ""
		if profile.IsAnchor {
			profile.GroupRoom = strconv.FormatInt(id, 10)
			if err := ensureAnchorPlans(ctx, tx, id, price); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET profile=$2 WHERE id=$1`, id, profile); err != nil {
			return err
		}
		action := "anchor_rejected"
		if profile.IsAnchor {
			action = "anchor_approved"
		}
		if _, err := tx.Exec(ctx, `INSERT INTO backoffice_logs(area,tenant,operator_id,operator_name,path,target,action,reason)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, actor.Area, actor.Scope(), actor.ID, actor.Name(), path, strconv.FormatInt(id, 10), action, reason); err != nil {
			return err
		}
		result, err = readAnchorReview(tx.QueryRow(ctx, "SELECT "+anchorReviewColumns+" FROM anchor_profiles a JOIN users u ON u.id=a.user_id WHERE a.user_id=$1", id))
		return err
	})
	return result, err
}

func (s *Store) EditTeacherLabel(ctx context.Context, actor admin.Principal, path string, id int64, label string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE rooms SET teacher_label=$2 WHERE id=$1 AND owner_id=$1`, id, label)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return admin.ErrBackofficeNotFound
		}
		_, err = tx.Exec(ctx, `INSERT INTO backoffice_logs(area,tenant,operator_id,operator_name,path,target,action,reason)
			VALUES($1,$2,$3,$4,$5,$6,'anchor_teacher_label',$7)`, actor.Area, actor.Scope(), actor.ID, actor.Name(), path,
			strconv.FormatInt(id, 10), label)
		return err
	})
}
