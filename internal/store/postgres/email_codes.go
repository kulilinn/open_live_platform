package postgres

import (
	"context"
	"crypto/rand"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"open_live_platform/internal/auth"
)

func (s *Store) FindByEmail(ctx context.Context, email string) (auth.User, bool, error) {
	var user auth.User
	err := s.pool.QueryRow(ctx, `SELECT id,email,account,language FROM users WHERE email=$1`, email).
		Scan(&user.ID, &user.Email, &user.Account, &user.Language)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.User{}, false, nil
	}
	return user, err == nil, err
}

func (s *Store) ReserveEmailCode(ctx context.Context, email string, userID int64, digest [32]byte, expiresAt time.Time) (auth.EmailCodeOffer, error) {
	// Bound the number of expired challenges cleared by an issuance request.
	if _, err := s.pool.Exec(ctx, `DELETE FROM auth_email_codes WHERE email IN (
		SELECT email FROM auth_email_codes WHERE expires_at<=now() ORDER BY expires_at LIMIT 32
		FOR UPDATE SKIP LOCKED
	)`); err != nil {
		return auth.EmailCodeOffer{}, err
	}
	var offer auth.EmailCodeOffer
	err := s.pool.QueryRow(ctx, `
		INSERT INTO auth_email_codes(email,user_id,code_digest,expires_at) VALUES($1,$2,$3,$4)
		ON CONFLICT(email) DO UPDATE SET user_id=EXCLUDED.user_id, code_digest=EXCLUDED.code_digest,
			expires_at=EXCLUDED.expires_at, delivered=FALSE, redeemed=FALSE, attempts=0
		WHERE auth_email_codes.expires_at<=now() OR auth_email_codes.redeemed OR auth_email_codes.user_id<>EXCLUDED.user_id
		RETURNING expires_at`, email, userID, digest[:], expiresAt).Scan(&offer.ExpiresAt)
	if err == nil {
		offer.New = true
		return offer, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return auth.EmailCodeOffer{}, err
	}
	err = s.pool.QueryRow(ctx, `SELECT expires_at,delivered FROM auth_email_codes WHERE email=$1 AND user_id=$2`, email, userID).
		Scan(&offer.ExpiresAt, &offer.Delivered)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.EmailCodeOffer{}, auth.ErrEmailCodeDelivery
	}
	return offer, err
}

func (s *Store) FinishEmailCode(ctx context.Context, email string, digest [32]byte, delivered bool) error {
	var tag pgconn.CommandTag
	var err error
	if delivered {
		tag, err = s.pool.Exec(ctx, `UPDATE auth_email_codes SET delivered=TRUE
			WHERE email=$1 AND code_digest=$2 AND NOT delivered AND expires_at>now()`, email, digest[:])
	} else {
		tag, err = s.pool.Exec(ctx, `DELETE FROM auth_email_codes WHERE email=$1 AND code_digest=$2 AND NOT delivered`, email, digest[:])
	}
	if err == nil && tag.RowsAffected() == 0 {
		return auth.ErrEmailCodeDelivery
	}
	return err
}

func (s *Store) RedeemEmailCode(ctx context.Context, email string, digest [32]byte, use auth.EmailCodeUse) (int64, bool, error) {
	var userID int64
	var redeemed bool
	err := s.pool.QueryRow(ctx, `UPDATE auth_email_codes SET attempts=attempts+1, redeemed=(code_digest=$2)
		WHERE email=$1 AND delivered AND NOT redeemed AND expires_at>now() AND attempts<$3
			AND (NOT $4 OR user_id=0) AND (NOT $5 OR user_id<>0)
		RETURNING user_id,redeemed`, email, digest[:], auth.EmailCodeMaxAttempts,
		use == auth.UseCodeForSignup, use == auth.UseCodeForReset).Scan(&userID, &redeemed)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	return userID, redeemed, err
}

func (s *Store) ResetVerifiedPassword(ctx context.Context, userID int64, email, password string) error {
	var salt [16]byte
	rand.Read(salt[:])
	key, err := auth.PasswordKey(password, salt[:])
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `UPDATE users SET password_salt=$1,password_hash=$2 WHERE id=$3 AND email=$4`, salt[:], key, userID, email)
	if err == nil && tag.RowsAffected() == 0 {
		return auth.ErrInvalidEmailCode
	}
	return err
}

func (s *Store) IssueVerifiedSession(ctx context.Context, user auth.User) (auth.Session, error) {
	token := rand.Text()
	tag, err := s.pool.Exec(ctx, `INSERT INTO sessions (token,user_id) SELECT $1,id FROM users WHERE id=$2 AND email=$3`, token, user.ID, user.Email)
	if err != nil {
		return auth.Session{}, err
	}
	if tag.RowsAffected() == 0 {
		return auth.Session{}, auth.ErrInvalidCredentials
	}
	return auth.Session{Token: token, User: user}, nil
}
