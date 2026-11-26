package postgres

import (
	"context"
	"crypto/rand"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"iter"
	"open_live_platform/internal/anchor"
	"open_live_platform/internal/auth"
	"open_live_platform/internal/live"
	"strconv"
	"strings"
)

var ErrAccountLedger = errors.New("account has retained financial records")

func (s *Store) DeleteAnchorAccount(ctx context.Context, email string) error {
	email = auth.NormalizeEmail(email)
	var id int64
	if err := s.pool.QueryRow(ctx, "SELECT id FROM users WHERE email=$1", email).Scan(&id); err != nil {
		return err
	}
	err := s.change(ctx, id, func(r *roomChange) error {
		var owner int64
		if err := r.tx.QueryRow(ctx, "SELECT id FROM users WHERE id=$1 AND email=$2 FOR UPDATE", id, email).Scan(&owner); err != nil {
			return err
		}
		if err := r.close(id); err != nil && !errors.Is(err, live.ErrRoomNotFound) {
			return err
		}
		if err := s.revokeBannedSessions(ctx, r.tx, id); err != nil {
			return err
		}
		for _, q := range []string{
			"DELETE FROM bot_chat_messages WHERE member_low=$1 OR member_high=$1",
			"DELETE FROM message_inbox WHERE owner_id=$1 OR peer_id=$1",
			"DELETE FROM message_config WHERE user_id=$1",
			"DELETE FROM notices WHERE receive_id=$1",
			"DELETE FROM feedback WHERE user_id=$1",
			"DELETE FROM users WHERE id=$1",
		} {
			if _, err := r.tx.Exec(ctx, q, id); err != nil {
				return err
			}
		}
		if _, err := r.tx.Exec(ctx, "DELETE FROM auth_email_codes WHERE email=$1", email); err != nil {
			return err
		}
		_, err := r.tx.Exec(ctx, `INSERT INTO backoffice_logs(area,tenant,operator_id,operator_name,path,target,action) VALUES('admin','operations',0,'maintenance','/anchor/api/delete_anchor', $1, 'delete_account')`, strconv.FormatInt(id, 10))
		return err
	})
	var constraint *pgconn.PgError
	if errors.As(err, &constraint) && constraint.Code == "23503" {
		return ErrAccountLedger
	}
	return err
}

func (s *Store) FillSearchNames(ctx context.Context) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(167884962,69)"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO unused_search_names(name) SELECT 'u'||lpad(floor(random()*100000000)::int::text,8,'0')
   FROM generate_series(1,1000) WHERE (SELECT count(*) FROM unused_search_names)<10000 ON CONFLICT DO NOTHING`)
		return err
	})
}

func (s *Store) RegisterAccounts(ctx context.Context, rows iter.Seq[auth.ProvisionUser], price int64) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		for row := range rows {
			email := auth.NormalizeEmail(row.Email)
			display := strings.TrimSpace(row.DisplayName)
			if email == "" || row.Password == "" || display == "" {
				return auth.ErrMissingFields
			}
			var u auth.User
			find := func() error {
				return tx.QueryRow(ctx, "SELECT id,account,profile FROM users WHERE email=$1 FOR UPDATE", email).Scan(&u.ID, &u.Account, &u.Profile)
			}
			err := find()
			if errors.Is(err, pgx.ErrNoRows) {
				var salt [16]byte
				rand.Read(salt[:])
				key, err := auth.PasswordKey(row.Password, salt[:])
				if err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `INSERT INTO users(email,account,password_salt,password_hash) VALUES($1,$2,$3,$4) ON CONFLICT(email) DO NOTHING`, email, display, salt[:], key); err != nil {
					return err
				}
				err = find()
				if err != nil {
					return err
				}
			} else if err != nil {
				return err
			}
			if !row.IsAnchor {
				continue
			}
			tag, err := tx.Exec(ctx, `INSERT INTO anchor_profiles(user_id,state) VALUES($1,$2) ON CONFLICT(user_id) DO NOTHING`, u.ID, anchor.Approved)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				continue
			}
			profile := auth.PublicProfile(u)
			profile.IsAnchor = true
			profile.GroupRoom = strconv.FormatInt(u.ID, 10)
			if _, err := tx.Exec(ctx, `UPDATE users SET profile=$2 WHERE id=$1`, u.ID, profile); err != nil {
				return err
			}
			if err := ensureAnchorPlans(ctx, tx, u.ID, price); err != nil {
				return err
			}
		}
		return nil
	})
}
func ensureAnchorPlans(ctx context.Context, tx pgx.Tx, id, price int64) error {
	if err := ensureFreePlan(ctx, tx, id); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO subscription_plans(anchor_id,plan_type,price)
  SELECT $1,3,$2 WHERE NOT EXISTS(SELECT 1 FROM subscription_plans WHERE anchor_id=$1 AND plan_type=3 AND NOT deleted)`, id, price)
	return err
}
