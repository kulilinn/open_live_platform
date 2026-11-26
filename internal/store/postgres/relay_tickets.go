package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/relay"
)

const relayTicketFields = `token,user_id,COALESCE(opaque_token,''),family_id,issuer,issued_at,expires_at,app_user,is_debug`

func scanRelayTicket(row pgx.Row) (relay.RustTicketRecord, error) {
	var ticket relay.RustTicketRecord
	err := row.Scan(&ticket.Token, &ticket.UserID, &ticket.Login.OpaqueToken, &ticket.Login.FamilyID,
		&ticket.Login.Issuer, &ticket.IssuedAt, &ticket.ExpiresAt, &ticket.Login.AppUser, &ticket.Debug)
	ticket.Login.UserID = ticket.UserID
	return ticket, err
}

func (s *Store) IssueRustRelayTicket(ctx context.Context, appKey string, candidate relay.RustTicketRecord, ttl time.Duration) (relay.RustTicketRecord, error) {
	// Opportunistic bounded cleanup keeps expired login families from accumulating.
	_, err := s.pool.Exec(ctx, `DELETE FROM relay_tickets WHERE (app_key,login_key) IN (
		SELECT app_key,login_key FROM relay_tickets WHERE expires_at<=statement_timestamp()
		ORDER BY expires_at LIMIT 100 FOR UPDATE SKIP LOCKED)`)
	if err != nil {
		return relay.RustTicketRecord{}, err
	}
	return scanRelayTicket(s.pool.QueryRow(ctx, `INSERT INTO relay_tickets AS t
		(app_key,login_key,token,user_id,opaque_token,family_id,issuer,issued_at,expires_at,app_user,is_debug)
		VALUES ($1,$2,$3,$4,NULLIF($5,''),$6,$7,$8,clock_timestamp()+make_interval(secs => $9),$10,$11)
		ON CONFLICT (app_key,login_key) DO UPDATE SET
			token=CASE WHEN t.expires_at<=statement_timestamp() THEN EXCLUDED.token ELSE t.token END,
			issued_at=CASE WHEN t.expires_at<=statement_timestamp() THEN EXCLUDED.issued_at ELSE t.issued_at END,
			expires_at=CASE WHEN t.expires_at<=statement_timestamp() THEN EXCLUDED.expires_at ELSE t.expires_at END
		RETURNING `+relayTicketFields,
		appKey, string(candidate.LoginKey), candidate.Token, candidate.UserID, candidate.Login.OpaqueToken,
		candidate.Login.FamilyID, candidate.Login.Issuer, candidate.IssuedAt, ttl.Seconds(), candidate.Login.AppUser, candidate.Debug))
}

func (s *Store) LookupRustRelayTicket(ctx context.Context, appKey, token string) (relay.RustTicketRecord, bool, error) {
	ticket, err := scanRelayTicket(s.pool.QueryRow(ctx, `SELECT `+relayTicketFields+` FROM relay_tickets
		WHERE app_key=$1 AND token=$2 AND expires_at>statement_timestamp()`, appKey, token))
	if errors.Is(err, pgx.ErrNoRows) {
		return relay.RustTicketRecord{}, false, nil
	}
	return ticket, err == nil, err
}

func (s *Store) RenewRustRelayTicket(ctx context.Context, appKey, token string, ttl time.Duration) error {
	_, err := s.pool.Exec(ctx, `UPDATE relay_tickets SET expires_at=clock_timestamp()+make_interval(secs => $3)
		WHERE app_key=$1 AND token=$2 AND expires_at>clock_timestamp()`, appKey, token, ttl.Seconds())
	return err
}
