package postgres

import (
	"context"
	"crypto/rand"
	"errors"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/auth"
)

func (s *Store) ResolveGoogleUser(ctx context.Context, subject, email, language string, mayLink bool) (id int64, created bool, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// Serialize the first login even if two verified tokens carry different email aliases.
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 316))", subject); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, "SELECT user_id FROM google_identities WHERE subject=$1", subject).Scan(&id)
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if !mayLink {
			return auth.ErrGoogleMailboxProof
		}
		err = tx.QueryRow(ctx, `INSERT INTO users(email,account,language,password_salt,password_hash)
			VALUES($1,$2,$3,''::bytea,''::bytea) ON CONFLICT(email) DO NOTHING RETURNING id`, email, "User_"+rand.Text()[:10], language).Scan(&id)
		created = err == nil
		if errors.Is(err, pgx.ErrNoRows) {
			err = tx.QueryRow(ctx, "SELECT id FROM users WHERE email=$1 FOR UPDATE", email).Scan(&id)
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "INSERT INTO google_identities(subject,user_id) VALUES($1,$2)", subject, id)
		if isUniqueViolation(err) {
			return auth.ErrGoogleToken
		}
		return err
	})
	return
}
