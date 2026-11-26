package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/auth"
)

const accountModerationColumns = `u.account_status,u.account_limit_until,COALESCE(v.limit_duration,0),COALESCE(v.reason,'')`
const accountModerationJoin = ` LEFT JOIN user_violation_records v ON v.id=u.account_violation_id AND v.user_id=u.id `
const activeAccountLimit = ` (u.account_limit_until=-1 OR (u.account_limit_until>0 AND u.account_limit_until>=floor(extract(epoch FROM clock_timestamp())))) `

func (s *Store) revokeBannedSessions(ctx context.Context, tx pgx.Tx, userID int64) error {
	_, err := tx.Exec(ctx, `WITH opaque AS (
		DELETE FROM sessions WHERE user_id=$1
		RETURNING 'opaque:'||encode(sha256(convert_to(token,'UTF8')),'hex') AS session_key
	), families AS (
		UPDATE jwt_sessions SET revoked_at=now() WHERE user_id=$1 AND revoked_at IS NULL
		RETURNING 'jwt:'||user_id::text||':'||family_id::text AS session_key
	)
	SELECT pg_notify($2,$3||session_key) FROM (SELECT * FROM opaque UNION ALL SELECT * FROM families UNION ALL SELECT $4) revoked`,
		userID, s.eventChannel, sessionRevocationPrefix, string(auth.RelayAppSessionKey(userID)))
	return err
}
