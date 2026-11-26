package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/auth"
)

func (s *Store) SaveLoginGrant(ctx context.Context, digest [32]byte, grant auth.LoginGrant) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM auth_login_grants WHERE code_digest IN (
		SELECT code_digest FROM auth_login_grants WHERE expires_at<=now() ORDER BY expires_at LIMIT 32
		FOR UPDATE SKIP LOCKED
	)`); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO auth_login_grants(code_digest,user_id,family_id,issuer,expires_at) VALUES($1,$2,$3,$4,$5)`,
		digest[:], grant.UserID, grant.FamilyID, grant.Issuer, grant.ExpiresAt)
	return err
}

func (s *Store) TakeLoginGrant(ctx context.Context, digest [32]byte) (auth.LoginGrant, bool, error) {
	var grant auth.LoginGrant
	err := s.pool.QueryRow(ctx, `DELETE FROM auth_login_grants WHERE code_digest=$1 AND expires_at>now()
		RETURNING user_id,family_id,issuer,expires_at`, digest[:]).Scan(&grant.UserID, &grant.FamilyID, &grant.Issuer, &grant.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.LoginGrant{}, false, nil
	}
	return grant, err == nil, err
}

func (s *Store) ReplaceJWTSession(ctx context.Context, grant auth.LoginGrant, user auth.User, newFamilyID int64) (bool, error) {
	key := auth.JWTSessionKey(grant.UserID, grant.FamilyID)
	tag, err := s.pool.Exec(ctx, `WITH replaced AS (
		UPDATE jwt_sessions js SET family_id=$3, expires_at=NULL, revoked_at=NULL, legacy_issuer=NULL
		FROM users u WHERE js.user_id=$1 AND js.family_id=$2 AND js.revoked_at IS NULL AND (js.expires_at IS NULL OR js.expires_at>now())
			AND u.id=js.user_id AND COALESCE(u.email,'')=$4 AND COALESCE(js.legacy_issuer,u.email,'')=$5
			AND NOT (u.account_status=2 AND `+activeAccountLimit+`) AND $6::timestamptz>now() RETURNING 1
	) SELECT pg_notify($7,$8) FROM replaced`, grant.UserID, grant.FamilyID, newFamilyID, user.Email, grant.Issuer, grant.ExpiresAt,
		s.eventChannel, sessionRevocationPrefix+string(key))
	replaced := err == nil && tag.RowsAffected() != 0
	if replaced {
		s.sessionRevocations.Revoke(key)
	}
	return replaced, err
}
