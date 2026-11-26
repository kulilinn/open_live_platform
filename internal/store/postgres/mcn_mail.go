package postgres

import (
	"context"
	"crypto/rand"
	"errors"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/admin"
)

func (s *Store) NextMCNInviteMail(ctx context.Context) (admin.MCNInviteMail, bool, error) {
	mail := admin.MCNInviteMail{Lease: rand.Text()}
	err := s.pool.QueryRow(ctx, `WITH next AS (
		SELECT q.id,u.email FROM mcn_invite_mail q
		JOIN backoffice_mcn_streamers m ON m.id=q.membership_id
		JOIN users u ON u.id=m.streamer_id WHERE q.sent_at IS NULL AND q.attempts<5
		  AND q.next_attempt<=statement_timestamp() AND (q.lease_until IS NULL OR q.lease_until<statement_timestamp())
		  AND m.status=4 AND NOT m.superseded AND m.invite_hash=q.invite_hash AND m.invite_until>statement_timestamp()
		  AND u.email IS NOT NULL AND u.email<>''
		ORDER BY q.next_attempt,q.id FOR UPDATE OF q SKIP LOCKED LIMIT 1
	)
	UPDATE mcn_invite_mail q SET lease_id=$1,lease_until=statement_timestamp()+interval '30 seconds',attempts=attempts+1
	FROM next WHERE q.id=next.id RETURNING q.id,next.email,q.subject,q.body`, mail.Lease).
		Scan(&mail.ID, &mail.Email, &mail.Subject, &mail.Body)
	if errors.Is(err, pgx.ErrNoRows) {
		return admin.MCNInviteMail{}, false, nil
	}
	return mail, err == nil, err
}

func (s *Store) CompleteMCNInviteMail(ctx context.Context, id int64, lease string, success bool) error {
	_, err := s.pool.Exec(ctx, `UPDATE mcn_invite_mail SET
		sent_at=CASE WHEN $3 THEN statement_timestamp() ELSE NULL END,
		next_attempt=statement_timestamp()+(1 << attempts)*interval '1 minute',lease_id=NULL,lease_until=NULL
		WHERE id=$1 AND lease_id=$2`, id, lease, success)
	return err
}
