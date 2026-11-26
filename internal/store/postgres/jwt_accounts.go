package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/auth"
)

func (s *Store) SaveJWTSession(ctx context.Context, userID, familyID int64) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO jwt_sessions(user_id, family_id) VALUES ($1, $2)
		ON CONFLICT (user_id) DO UPDATE SET family_id=EXCLUDED.family_id`,
		userID, familyID)
	return err
}

func (s *Store) JWTUser(ctx context.Context, userID, familyID int64, email string) (auth.User, bool, error) {
	var user auth.User
	err := s.pool.QueryRow(ctx, `
		SELECT u.id, u.email, u.account, u.language
		FROM jwt_sessions js JOIN users u ON u.id=js.user_id
		WHERE js.user_id=$1 AND js.family_id=$2 AND u.email=$3`,
		userID, familyID, email).Scan(&user.ID, &user.Email, &user.Account, &user.Language)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.User{}, false, nil
	}
	return user, err == nil, err
}

func (s *Store) RevokeJWTSession(ctx context.Context, userID, familyID int64) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM jwt_sessions WHERE user_id=$1 AND family_id=$2`, userID, familyID)
	return err
}
