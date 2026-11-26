package postgres

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"open_live_platform/internal/admin"
	"open_live_platform/internal/auth"
	"open_live_platform/internal/live"
)

var _ admin.BackofficeStore = (*Store)(nil)

func (s *Store) LoginBackoffice(ctx context.Context, session admin.BackofficeSession) error {
	if session.DatabaseVersion == "" && session.Area != admin.MCNArea {
		return loginBackoffice(ctx, s.pool, session)
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockSystemArea(ctx, tx, session.Area); err != nil {
			return err
		}
		if session.Area == admin.MCNArea {
			var disabled bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM mcn_agency_directory WHERE id=$1 AND disabled)`, session.AgencyID).Scan(&disabled); err != nil {
				return err
			}
			if disabled {
				return admin.ErrBackofficeNotFound
			}
		}
		if session.DatabaseVersion == "" {
			return loginBackoffice(ctx, tx, session)
		}
		var current bool
		err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM backoffice_system_accounts
			WHERE area=$1 AND id=$2 AND credential_version::text=$3 AND NOT disabled AND NOT deleted)`,
			session.Area, session.ID, session.DatabaseVersion).Scan(&current)
		if err != nil {
			return err
		}
		if !current {
			return admin.ErrBackofficeNotFound
		}
		return loginBackoffice(ctx, tx, session)
	})
}

func loginBackoffice(ctx context.Context, db interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, session admin.BackofficeSession) error {
	_, err := db.Exec(ctx, `WITH logged_in AS (INSERT INTO backoffice_sessions(area,principal_id,fingerprint,access_hash,refresh_hash,access_expires,refresh_expires)
		VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(area,principal_id) DO UPDATE SET fingerprint=EXCLUDED.fingerprint,
		access_hash=EXCLUDED.access_hash,refresh_hash=EXCLUDED.refresh_hash,access_expires=EXCLUDED.access_expires,refresh_expires=EXCLUDED.refresh_expires RETURNING principal_id)
		INSERT INTO mcn_messages(agency_id,manager_id,user_id,ty,params)
		SELECT $8,CASE WHEN $9::bool THEN $2::bigint END,principal_id,3,jsonb_build_object('name',$10::text,'ty',$11::smallint)
		FROM logged_in WHERE $1='mcn'`,
		session.Area, session.ID, session.Fingerprint[:], session.AccessHash[:], session.RefreshHash[:], session.AccessExpires, session.RefreshExpires,
		session.AgencyID, session.Manager, session.DisplayName, session.AccountType)
	return err
}

func (s *Store) FindBackoffice(ctx context.Context, area string, hash [sha256.Size]byte, refresh bool) (int64, [sha256.Size]byte, error) {
	var id int64
	var stored []byte
	condition := "access_hash=$2 AND access_expires>statement_timestamp()"
	if refresh {
		condition = "refresh_hash=$2 AND refresh_expires>statement_timestamp()"
	}
	err := s.pool.QueryRow(ctx, `SELECT principal_id,fingerprint FROM backoffice_sessions WHERE area=$1 AND `+condition, area, hash[:]).Scan(&id, &stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, [sha256.Size]byte{}, admin.ErrBackofficeNotFound
	}
	if err != nil {
		return 0, [sha256.Size]byte{}, err
	}
	if len(stored) != sha256.Size {
		return 0, [sha256.Size]byte{}, admin.ErrBackofficeNotFound
	}
	return id, [sha256.Size]byte(stored), nil
}

func (s *Store) RefreshBackoffice(ctx context.Context, session admin.BackofficeSession, old [sha256.Size]byte) error {
	result, err := s.pool.Exec(ctx, `UPDATE backoffice_sessions SET access_hash=$4,refresh_hash=$5,access_expires=$6,refresh_expires=$7
		WHERE area=$1 AND principal_id=$2 AND refresh_hash=$3 AND fingerprint=$8 AND refresh_expires>statement_timestamp()`,
		session.Area, session.ID, old[:], session.AccessHash[:], session.RefreshHash[:], session.AccessExpires, session.RefreshExpires, session.Fingerprint[:])
	if err == nil && result.RowsAffected() == 0 {
		return admin.ErrBackofficeNotFound
	}
	return err
}

func (s *Store) LogoutBackoffice(ctx context.Context, area string, access [sha256.Size]byte) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM backoffice_sessions WHERE area=$1 AND access_hash=$2`, area, access[:])
	return err
}

func (s *Store) AuditBackofficeAccess(ctx context.Context, p admin.Principal, requestPath string) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO backoffice_logs(area,tenant,operator_id,operator_name,path)
	    VALUES($1,$2,$3,$4,$5)`, p.Area, p.Scope(), p.ID, p.Name(), requestPath)
	return err
}

func readBackofficePage[T any](ctx context.Context, s *Store, page admin.PageRequest, columns, relation, ordering string,
	params []any, scan func(pgx.CollectableRow) (T, error)) ([]T, int64, error) {
	result := []T{}
	var total int64
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, "SELECT count(*) "+relation, params...).Scan(&total); err != nil {
			return err
		}
		_, limit, offset := page.Window()
		if offset >= total {
			return nil
		}
		paging := fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(params)+1, len(params)+2)
		rows, err := tx.Query(ctx, "SELECT "+columns+" "+relation+" ORDER BY "+ordering+paging, append(params, limit, offset)...)
		if err != nil {
			return err
		}
		result, err = pgx.CollectRows(rows, scan)
		return err
	})
	return result, total, err
}

const backofficeAccountStatus = `(CASE WHEN ` + activeAccountLimit + ` THEN u.account_status ELSE 0 END)`

func (s *Store) PlatformStats(ctx context.Context, start, end time.Time) (admin.PlatformStats, error) {
	var p admin.PlatformStats
	err := s.pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE created_at BETWEEN $1 AND $2),
		(SELECT count(*) FROM user_login_activity WHERE last_login_at BETWEEN $1 AND $2),
		(SELECT count(*) FROM (SELECT user_id FROM sessions UNION SELECT j.user_id FROM jwt_sessions j
			LEFT JOIN user_login_activity a ON a.user_id=j.user_id WHERE j.revoked_at IS NULL AND
			coalesce(j.expires_at,a.last_login_at+interval '24 hours')>now()) active),
		(SELECT count(*) FROM rooms WHERE online) FROM users`, start, end).Scan(&p.Users, &p.NewUsers, &p.Logins, &p.Online, &p.Streams)
	return p, err
}

const backofficeRoomBanned = `(r.moderation_until=-1 OR (r.moderation_until>0 AND r.moderation_until>=floor(extract(epoch FROM clock_timestamp()))))`

func (s *Store) BackofficeUsers(ctx context.Context, query admin.UserQuery) ([]admin.UserRow, int64, error) {
	const columns = `u.id, COALESCE(u.email,u.legacy_profile_email,''), u.account, u.profile->>'SearchName', u.profile->>'Photo',
		(u.profile->>'Gender')::integer, u.created_at, a.last_login_at,(ledger.record->>'point')::bigint,
		(ledger.record->>'total_used_point')::bigint,(ledger.record->>'total_expired_point')::bigint,(ledger.record->>'history_total')::bigint,
		CASE WHEN COALESCE((u.profile->>'IsAnchor')::boolean,r.id IS NOT NULL) THEN 1 ELSE 0 END,
		` + backofficeAccountStatus + `, CASE WHEN r.id IS NULL THEN NULL WHEN ` + backofficeRoomBanned + ` THEN CASE WHEN r.moderation_until=-1 THEN -1 ELSE 1 END ELSE 0 END,
		CASE WHEN ` + backofficeRoomBanned + ` AND r.moderation_until>0 THEN to_timestamp(r.moderation_until) END`
	const relation = `FROM users u LEFT JOIN rooms r ON r.id=u.id LEFT JOIN user_login_activity a ON a.user_id=u.id
		LEFT JOIN legacy_business_history ledger ON ledger.source_table='user_points' AND ledger.source_id=u.id::text
		WHERE ($1::bigint IS NULL OR u.id=$1)
		AND ($2::text IS NULL OR strpos(lower(u.account),lower($2))>0)
		AND ($3::text IS NULL OR strpos(lower(COALESCE(u.email,u.legacy_profile_email,'')),lower($3))>0)
		AND ($4::smallint IS NULL OR ` + backofficeAccountStatus + `=$4)
		AND ($5::timestamptz IS NULL OR a.last_login_at >= $5) AND ($6::timestamptz IS NULL OR a.last_login_at <= $6)`
	return readBackofficePage(ctx, s, query.PageRequest, columns, relation, "u.id DESC", []any{query.ID, query.Nickname, query.Email, query.Status, query.Start, query.End},
		func(row pgx.CollectableRow) (admin.UserRow, error) {
			var item admin.UserRow
			err := row.Scan(&item.ID, &item.Email, &item.DisplayName, &item.SearchName, &item.Photo, &item.Gender, &item.CreatedAt,
				&item.LastLogin, &item.Point, &item.UsedPoint, &item.ExpiredPoint, &item.HistoryPoints,
				&item.IsStreamer, &item.Status, &item.RoomLimited, &item.RoomLimitedAt)
			return item, err
		})
}

func (s *Store) BackofficeRooms(ctx context.Context, query admin.RoomQuery) ([]admin.RoomRow, int64, error) {
	const columns = `r.id,r.live_id,r.owner_id,u.account,(u.profile->>'Gender')::integer,
		(SELECT count(*) FROM room_members m WHERE m.room_id=r.id),
		(SELECT COALESCE(sum((g.request->>'quantity')::bigint),0)::bigint FROM gift_orders g WHERE g.room_id=r.id AND g.live_id=r.live_id AND g.state=1),
		CASE WHEN ` + backofficeRoomBanned + ` THEN 4 WHEN r.anchor_away THEN 3 WHEN r.online THEN 1 ELSE 0 END,
		CASE WHEN ` + backofficeRoomBanned + ` THEN CASE WHEN r.moderation_until=-1 THEN -1 ELSE 1 END ELSE 0 END,
		CASE WHEN ` + backofficeRoomBanned + ` AND r.moderation_until>0 THEN to_timestamp(r.moderation_until) END,
		r.banner,r.live_started_at,r.live_ended_at,` + backofficeAccountStatus + `,u.account_limit_until`
	const relation = `FROM rooms r JOIN users u ON u.id=r.owner_id WHERE ($1::bigint IS NULL OR r.id=$1)
		AND ($2::bigint IS NULL OR r.owner_id=$2) AND ($3::text IS NULL OR strpos(lower(u.account),lower($3))>0)
		AND ($4::text IS NULL OR strpos(lower(COALESCE(u.email,u.legacy_profile_email,'')),lower($4))>0)`
	return readBackofficePage(ctx, s, query.PageRequest, columns, relation, "r.online DESC,r.id DESC", []any{query.RoomID, query.StreamerID, query.Name, query.Email},
		func(row pgx.CollectableRow) (admin.RoomRow, error) {
			var item admin.RoomRow
			var ended *time.Time
			err := row.Scan(&item.RoomID, &item.LiveID, &item.StreamerID, &item.Name, &item.Gender, &item.Members, &item.Gifts, &item.Online,
				&item.Limited, &item.LimitedAt, &item.Banner, &item.LiveTime, &ended, &item.AccountStatus, &item.AccountLimit)
			item.ChatID, item.RoomType = item.RoomID, "1vN"
			if item.LiveTime != nil {
				session := live.LiveSession{StartedAt: *item.LiveTime}
				if ended != nil {
					session.EndedAt = *ended
				}
				item.LiveDuration = int64(session.Duration(time.Now()) / time.Second)
				item.OnlineTime = item.LiveDuration
			}
			return item, err
		})
}

func (s *Store) BackofficeViolations(ctx context.Context, userID int64, page admin.PageRequest) ([]admin.Violation, int64, error) {
	return readBackofficePage(ctx, s, page, "user_id,reason,limit_duration,ty,create_time,create_by_name", "FROM user_violation_records WHERE user_id=$1",
		"create_time DESC,id DESC", []any{userID}, func(row pgx.CollectableRow) (admin.Violation, error) {
			var result admin.Violation
			err := row.Scan(&result.UserID, &result.Reason, &result.Duration, &result.Type, &result.Created, &result.ActorName)
			return result, err
		})
}

func (s *Store) BackofficeLogs(ctx context.Context, p admin.Principal, page admin.PageRequest) ([]admin.Log, int64, error) {
	return readBackofficePage(ctx, s, page, "target,action,operator_id,operator_name,create_time", "FROM backoffice_logs WHERE area=$1 AND tenant=$2 AND action<>''",
		"create_time DESC,id DESC", []any{p.Area, p.Scope()}, func(row pgx.CollectableRow) (admin.Log, error) {
			var result admin.Log
			err := row.Scan(&result.Target, &result.Action, &result.ActorID, &result.ActorName, &result.Created)
			return result, err
		})
}

func (s *Store) ApplyBackofficeAction(ctx context.Context, actor admin.Principal, op admin.Action) error {
	id := op.UserID
	if op.RoomID != 0 {
		id = op.RoomID
	}
	return s.change(ctx, id, func(r *roomChange) error {
		return s.applyBackofficeAction(ctx, r, actor, op)
	})
}

func (s *Store) applyBackofficeAction(ctx context.Context, r *roomChange, actor admin.Principal, op admin.Action) error {
	var userID int64
	if err := r.tx.QueryRow(ctx, "SELECT id FROM users WHERE id=$1 FOR UPDATE", op.UserID).Scan(&userID); errors.Is(err, pgx.ErrNoRows) {
		return admin.ErrBackofficeNotFound
	} else if err != nil {
		return err
	}
	if op.RoomID != 0 {
		var ownerID int64
		err := r.tx.QueryRow(ctx, "SELECT owner_id FROM rooms WHERE id=$1", op.RoomID).Scan(&ownerID)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && ownerID != op.UserID) {
			return admin.ErrBackofficeNotFound
		}
		if err != nil {
			return err
		}
	}
	reason := strings.Join(op.Reason, ",")
	var recordID int64
	err := r.tx.QueryRow(ctx, `INSERT INTO backoffice_logs (area,tenant,operator_id,operator_name,path,target,action,reason)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`, actor.Area, actor.Scope(), actor.ID, actor.Name(), op.Path,
		strconv.FormatInt(op.UserID, 10), op.Kind, reason).Scan(&recordID)
	if err != nil {
		return err
	}
	if ty := map[string]int{"account_disable": 1, "no_talking": 2, "account_reset": 3, "streamer_alert": 4, "streamer_end_stream": 5, "streamer_disable": 6}[op.Kind]; ty != 0 {
		if _, err := r.tx.Exec(ctx, `INSERT INTO user_violation_records (id,user_id,reason,limit_duration,create_time,create_by_name,ty)
				VALUES($1,$2,$3,$4,statement_timestamp(),$5,$6)`, recordID, op.UserID, reason, op.Duration, actor.Name(), ty); err != nil {
			return err
		}
	}
	until := op.Duration
	if until > 0 {
		until += time.Now().Unix()
	}
	switch op.Kind {
	case "streamer_alert":
		_, err := r.tx.Exec(ctx, `INSERT INTO notices(id,notice_target,notice_type,receive_id,content,create_time)
				VALUES(nextval('local_notice_ids'),1,12,$1,jsonb_build_object('reason',coalesce($2::text[],'{}')),now())`, op.UserID, op.Reason)
		return err
	case "account_reset":
		if _, err := r.tx.Exec(ctx, `UPDATE users SET account='',profile=coalesce(profile,'{}'::jsonb)||'{"Name":"","Photo":""}' WHERE id=$1`, op.UserID); err != nil {
			return err
		}
		_, err := r.tx.Exec(ctx, `INSERT INTO notices(id,notice_target,notice_type,receive_id,content,create_time)
				VALUES(nextval('local_notice_ids'),3,16,$1,jsonb_build_object('reason',coalesce($2::text[],'{}')),now())`, op.UserID, op.Reason)
		return err
	case "account_disable", "account_enable", "no_talking":
		status := auth.AccountNormal
		switch op.Kind {
		case "account_disable":
			status = auth.AccountBanned
		case "no_talking":
			status = auth.AccountMuted
		case "account_enable":
			recordID, until = 0, 0
		}
		if _, err := r.tx.Exec(ctx, `UPDATE users SET account_status=$2,account_limit_until=$3,account_violation_id=$4 WHERE id=$1`, op.UserID, status, until, recordID); err != nil {
			return err
		}
		if status != auth.AccountBanned {
			return nil
		}
		if err := s.revokeBannedSessions(ctx, r.tx, op.UserID); err != nil {
			return err
		}
		if err := r.close(op.UserID); !errors.Is(err, live.ErrRoomNotFound) {
			return err
		}
		return nil
	case "streamer_disable", "streamer_enable":
		if op.Kind == "streamer_enable" {
			until, op.Duration, reason = 0, 0, ""
		}
		if _, err := r.tx.Exec(ctx, `UPDATE rooms SET moderation_until=$2,moderation_total=$3,moderation_reason=$4 WHERE id=$1`, op.RoomID, until, op.Duration, reason); err != nil {
			return err
		}
		if op.Kind == "streamer_enable" {
			return nil
		}
		return r.close(op.UserID)
	case "streamer_end_stream":
		return r.close(op.UserID)
	default:
		return errors.New("unknown backoffice action")
	}
}

func (s *Store) BackofficeStreamers(ctx context.Context, p admin.Principal, query admin.StreamerQuery) ([]admin.StreamerRow, int64, error) {
	const relation = `FROM backoffice_mcn_streamers WHERE mcn_id=$1 AND NOT superseded AND ($2::bigint IS NULL OR manager_id=$2)
		AND ($3::text IS NULL OR strpos(lower(name),lower($3))>0) AND ($4::text IS NULL OR strpos(lower(email),lower($4))>0)
		AND ($5::smallint[] IS NULL OR status=ANY($5)) AND ($6::text IS NULL OR (create_time AT TIME ZONE 'UTC')::date=$6::text::date)`
	const columns = `id,streamer_id,manager_id,email,name,manager_name,ratio::text,manager_ratio::text,agency_ratio::text,fan_num,status,create_time`
	args := []any{p.AgencyID, query.ManagerID, query.Name, query.Email, query.Status, query.Date}
	scan := func(row pgx.CollectableRow) (admin.StreamerRow, error) {
		var result admin.StreamerRow
		err := row.Scan(&result.ID, &result.StreamerID, &result.ManagerID, &result.Email, &result.Name, &result.ManagerName,
			&result.Ratio, &result.ManagerRatio, &result.AgencyRatio, &result.Fans, &result.Status, &result.Created)
		return result, err
	}
	if !query.All {
		return readBackofficePage(ctx, s, query.PageRequest, columns, relation, "create_time DESC,id DESC", args, scan)
	}
	rows, err := s.pool.Query(ctx, "SELECT "+columns+" "+relation+" ORDER BY create_time DESC,id DESC", args...)
	if err != nil {
		return nil, 0, err
	}
	list, err := pgx.CollectRows(rows, scan)
	return list, int64(len(list)), err
}

func (s *Store) BackofficeGiftPurchases(ctx context.Context, query admin.PurchaseQuery) ([]admin.PurchaseRow, int64, error) {
	const relation = `FROM gift_orders g LEFT JOIN finance_bad_debt_receipts debt ON debt.provider_order_id=g.provider_order_id
		AND debt.provider_order_no=g.receipt->>'order_no' AND debt.spender_id=g.sender_id AND debt.charged_points=g.cost
		WHERE state=1 AND ($1::bigint IS NULL OR receiver_id=$1) AND ($2::bigint IS NULL OR sender_id=$2)
		AND ($3::bigint IS NULL OR receipt->>'order_no'=$3::bigint::text) AND ($4::smallint IS NULL OR $4=2)
		AND ($5::text IS NULL OR starts_with(item->>'name',$5)) AND ($6::timestamptz IS NULL OR paid_at>=$6)
		AND ($7::timestamptz IS NULL OR paid_at<=$7)`
	const debt = `COALESCE(debt.bad_debt_points,(receipt->>'total_bad_debt_point')::bigint,0)`
	const columns = `cost,receiver_id,sender_id,COALESCE(receipt->>'order_no',''),income,` + debt + `,
		COALESCE(trunc(income::numeric*` + debt + `/NULLIF(cost,0))::bigint,0),item->>'name',paid_at`
	args := []any{query.ReceiverID, query.SenderID, query.TradeNo, query.TradeType, query.GiftName, query.Start, query.End}
	return readBackofficePage(ctx, s, query.PageRequest, columns, relation, "paid_at DESC,id DESC", args, func(row pgx.CollectableRow) (admin.PurchaseRow, error) {
		result := admin.PurchaseRow{TradeType: 2}
		err := row.Scan(&result.Points, &result.Receiver, &result.Sender, &result.TradeNo, &result.Income, &result.BadDebt,
			&result.BadIncome, &result.Subject, &result.Created)
		return result, err
	})
}
