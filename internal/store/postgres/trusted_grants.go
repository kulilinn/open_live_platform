package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/auth"
)

const trustedSessionAvailable = `s.revoked_at IS NULL AND (s.expires_at IS NULL OR s.expires_at>now())
	AND NOT (u.account_status=2 AND ` + activeAccountLimit + `)`

func (s *Store) SaveTrustedGrant(ctx context.Context, digest [32]byte, grant auth.TrustedGrant) (bool, error) {
	if _, err := s.pool.Exec(ctx, `DELETE FROM trusted_auth_grants WHERE code_digest IN
		(SELECT code_digest FROM trusted_auth_grants WHERE expires_at<=now() ORDER BY expires_at LIMIT 32 FOR UPDATE SKIP LOCKED)`); err != nil {
		return false, err
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO trusted_auth_grants(code_digest,purpose,client_id,redirect_uri,user_id,family_id,issuer,expires_at)
		SELECT $1,$2,$3,$4,u.id,s.family_id,$7,$8 FROM jwt_sessions s JOIN users u ON u.id=s.user_id
		WHERE s.user_id=$5 AND s.family_id=$6 AND COALESCE(s.legacy_issuer,u.email,'')=$7 AND $8>now() AND `+trustedSessionAvailable,
		digest[:], grant.Purpose, grant.ClientID, grant.Redirect, grant.UserID, grant.FamilyID, grant.Issuer, grant.ExpiresAt)
	return tag.RowsAffected() == 1, err
}

func (s *Store) ConsumeTrustedGrant(ctx context.Context, digest [32]byte, binding auth.GrantBinding) (int64, error) {
	var uid int64
	err := s.pool.QueryRow(ctx, `DELETE FROM trusted_auth_grants g USING jwt_sessions s,users u
		WHERE g.code_digest=$1 AND g.purpose=$2 AND g.client_id=$3 AND g.redirect_uri=$4
		AND ($5::bigint=0 OR g.user_id=$5) AND g.expires_at>now()
		AND s.user_id=g.user_id AND s.family_id=g.family_id AND u.id=g.user_id
		AND COALESCE(s.legacy_issuer,u.email,'')=g.issuer AND `+trustedSessionAvailable+`
		RETURNING g.user_id`, digest[:], binding.Purpose, binding.ClientID, binding.Redirect, binding.UserID).Scan(&uid)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, auth.ErrTrustedGrant
	}
	return uid, err
}
