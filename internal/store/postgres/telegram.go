package postgres

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/auth"
)

func (s *Store) ResolveTelegramUser(ctx context.Context, id int64, name, language string) (auth.User, error) {
	var bound bool
	err := s.pool.QueryRow(ctx, "SELECT telegram_identity FROM users WHERE id=$1", id).Scan(&bound)
	if errors.Is(err, pgx.ErrNoRows) {
		err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			// Legacy Telegram accounts use the Telegram ID as the platform ID.
			if _, err := tx.Exec(ctx, "LOCK TABLE users IN SHARE ROW EXCLUSIVE MODE"); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO users(id,account,language,password_salt,password_hash,telegram_identity) VALUES($1,$2,$3,''::bytea,''::bytea,true)
				ON CONFLICT(id) DO NOTHING`, id, name, language); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, "SELECT telegram_identity FROM users WHERE id=$1", id).Scan(&bound); err != nil {
				return err
			}
			if !bound {
				return auth.ErrTelegramCollision
			}
			_, err := tx.Exec(ctx, `SELECT setval('users_id_seq',$1,true) FROM users_id_seq WHERE last_value<$1 OR (last_value=$1 AND NOT is_called)`, id)
			return err
		})
	}
	if err != nil {
		return auth.User{}, err
	}
	if !bound {
		return auth.User{}, auth.ErrTelegramCollision
	}
	u, _, err := s.Find(ctx, id)
	return u, err
}
