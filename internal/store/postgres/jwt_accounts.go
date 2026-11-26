package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"open_live_platform/internal/auth"
)

func (s *Store) SaveJWTSession(ctx context.Context, userID, familyID int64) error {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO jwt_sessions(user_id, family_id)
		SELECT u.id,$2 FROM users u WHERE u.id=$1 AND NOT (u.account_status=2 AND `+activeAccountLimit+`) FOR SHARE
		ON CONFLICT (user_id) DO UPDATE SET
			family_id=EXCLUDED.family_id, expires_at=NULL, revoked_at=NULL, legacy_issuer=NULL`,
		userID, familyID)
	if err == nil && tag.RowsAffected() == 0 {
		return s.sessionIssueError(ctx, userID)
	}
	return err
}

func (s *Store) JWTUser(ctx context.Context, userID, familyID int64, email string) (auth.User, bool, error) {
	var user auth.User
	err := s.pool.QueryRow(ctx, `
		SELECT u.id, COALESCE(u.email,''), u.account, u.language, `+accountModerationColumns+`
		FROM jwt_sessions js JOIN users u ON u.id=js.user_id`+accountModerationJoin+`
		WHERE js.user_id=$1 AND js.family_id=$2 AND COALESCE(js.legacy_issuer, u.email,'')=$3
			AND (js.legacy_issuer IS NULL OR u.legacy_profile_email=js.legacy_issuer OR COALESCE(u.email,'')=$4)
			AND js.revoked_at IS NULL AND (js.expires_at IS NULL OR js.expires_at > now())`,
		userID, familyID, email, auth.NormalizeEmail(email)).Scan(&user.ID, &user.Email, &user.Account, &user.Language,
		&user.Moderation.Type, &user.Moderation.Until, &user.Moderation.Total, &user.Moderation.Reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.User{}, false, nil
	}
	return user, err == nil, err
}

type LegacyJWTImportResult string

const (
	LegacyJWTImported        LegacyJWTImportResult = "imported"
	LegacyJWTExpired         LegacyJWTImportResult = "expired"
	LegacyJWTAccountMismatch LegacyJWTImportResult = "account_mismatch"
	LegacyJWTExistingSession LegacyJWTImportResult = "existing_session"
	LegacyJWTRestricted      LegacyJWTImportResult = "restricted"
)

// ImportLegacyJWTSession imports the cached Rust family if the original account
// exists and no Go-side login, import or revocation has already claimed its row.
func (s *Store) ImportLegacyJWTSession(ctx context.Context, session auth.LegacyJWTSession) (LegacyJWTImportResult, error) {
	var result LegacyJWTImportResult
	err := s.pool.QueryRow(ctx, `
		WITH account AS (SELECT id, (u.account_status=2 AND `+activeAccountLimit+`) banned FROM users u
			WHERE id=$1 AND (COALESCE(email,'')=$5 OR legacy_profile_email=$2)),
		inserted AS (
			INSERT INTO jwt_sessions(user_id, family_id, expires_at, legacy_issuer)
			SELECT id, $3, $4::timestamptz, $2 FROM account WHERE $4::timestamptz > now() AND NOT banned
			ON CONFLICT (user_id) DO NOTHING RETURNING 1
		)
		SELECT CASE
			WHEN EXISTS (SELECT 1 FROM inserted) THEN 'imported'
			WHEN $4::timestamptz <= now() THEN 'expired'
			WHEN NOT EXISTS (SELECT 1 FROM account) THEN 'account_mismatch'
			WHEN EXISTS (SELECT 1 FROM account WHERE banned) THEN 'restricted'
			ELSE 'existing_session'
		END`, session.UserID, session.Email, session.FamilyID, session.ExpiresAt, auth.NormalizeEmail(session.Email)).Scan(&result)
	return result, err
}

func (s *Store) RevokeJWTSession(ctx context.Context, userID, familyID int64) error {
	key := auth.JWTSessionKey(userID, familyID)
	tag, err := s.pool.Exec(ctx, `
		WITH revoked AS (
			UPDATE jwt_sessions SET revoked_at=now()
			WHERE user_id=$1 AND family_id=$2 AND revoked_at IS NULL RETURNING 1
		)
		SELECT pg_notify($3,$4) FROM revoked`, userID, familyID, s.eventChannel, sessionRevocationPrefix+string(key))
	if err == nil && tag.RowsAffected() != 0 {
		s.sessionRevocations.Revoke(key)
	}
	return err
}
