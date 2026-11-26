package postgres

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"open_live_platform/internal/auth"
)

func (s *Store) SignUp(ctx context.Context, email, password, account string) (auth.Session, error) {
	email = auth.NormalizeEmail(email)
	account = strings.TrimSpace(account)
	if email == "" || password == "" || account == "" {
		return auth.Session{}, auth.ErrMissingFields
	}

	salt := make([]byte, 16)
	rand.Read(salt)
	key, err := auth.PasswordKey(password, salt)
	if err != nil {
		return auth.Session{}, err
	}
	token := rand.Text()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return auth.Session{}, err
	}
	defer tx.Rollback(ctx)

	var user auth.User
	err = tx.QueryRow(ctx, `
		INSERT INTO users (email, account, password_salt, password_hash)
		VALUES ($1, $2, $3, $4)
		RETURNING id, email, account, language`, email, account, salt, key).
		Scan(&user.ID, &user.Email, &user.Account, &user.Language)
	if isUniqueViolation(err) {
		return auth.Session{}, auth.ErrEmailInUse
	}
	if err != nil {
		return auth.Session{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO sessions (token, user_id) VALUES ($1, $2)`, token, user.ID); err != nil {
		return auth.Session{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return auth.Session{}, err
	}
	return auth.Session{Token: token, User: user}, nil
}

func (s *Store) SignIn(ctx context.Context, email, password string) (auth.Session, error) {
	var user auth.User
	var salt, storedKey []byte
	err := s.pool.QueryRow(ctx, `
		SELECT id, email, account, language, password_salt, password_hash
		FROM users WHERE email = $1`, auth.NormalizeEmail(email)).
		Scan(&user.ID, &user.Email, &user.Account, &user.Language, &salt, &storedKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.Session{}, auth.ErrInvalidCredentials
	}
	if err != nil {
		return auth.Session{}, err
	}
	key, err := auth.PasswordKey(password, salt)
	if err != nil {
		return auth.Session{}, err
	}
	if subtle.ConstantTimeCompare(key, storedKey) != 1 {
		return auth.Session{}, auth.ErrInvalidCredentials
	}

	token := rand.Text()
	if _, err := s.pool.Exec(ctx, `INSERT INTO sessions (token, user_id) VALUES ($1, $2)`, token, user.ID); err != nil {
		return auth.Session{}, err
	}
	return auth.Session{Token: token, User: user}, nil
}

func (s *Store) Lookup(ctx context.Context, token string) (auth.User, bool, error) {
	var user auth.User
	err := s.pool.QueryRow(ctx, `
		SELECT u.id, u.email, u.account, u.language
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token = $1`, token).Scan(&user.ID, &user.Email, &user.Account, &user.Language)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.User{}, false, nil
	}
	return user, err == nil, err
}

func (s *Store) Bearer(ctx context.Context, authorization string) (auth.User, bool, error) {
	token, found := strings.CutPrefix(authorization, "Bearer ")
	if !found {
		return auth.User{}, false, nil
	}
	return s.Lookup(ctx, token)
}

func (s *Store) Refresh(ctx context.Context, token string) (auth.Session, bool, error) {
	var user auth.User
	err := s.pool.QueryRow(ctx, `
		SELECT u.id, u.email, u.account, u.language
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token = $1`, token,
	).Scan(&user.ID, &user.Email, &user.Account, &user.Language)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.Session{}, false, nil
	}
	if err != nil {
		return auth.Session{}, false, err
	}
	newToken := rand.Text()
	if _, err := s.pool.Exec(ctx, `INSERT INTO sessions (token, user_id) VALUES ($1, $2)`, newToken, user.ID); err != nil {
		return auth.Session{}, false, err
	}
	return auth.Session{Token: newToken, User: user}, true, nil
}

func (s *Store) Revoke(ctx context.Context, token string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE token = $1`, token)
	return err
}

func (s *Store) Find(ctx context.Context, id int64) (auth.User, bool, error) {
	var user auth.User
	err := s.pool.QueryRow(ctx,
		`SELECT id, email, account, language FROM users WHERE id = $1`, id,
	).Scan(&user.ID, &user.Email, &user.Account, &user.Language)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.User{}, false, nil
	}
	return user, err == nil, err
}

func (s *Store) SetLanguage(ctx context.Context, id int64, language string) error {
	_, err := s.pool.Exec(ctx, `UPDATE users SET language = $2 WHERE id = $1`, id, language)
	return err
}

func (s *Store) ChangePassword(ctx context.Context, id int64, oldPassword, newPassword string) error {
	if oldPassword == "" || newPassword == "" {
		return auth.ErrMissingFields
	}
	var salt, storedKey []byte
	err := s.pool.QueryRow(ctx,
		`SELECT password_salt, password_hash FROM users WHERE id = $1`, id,
	).Scan(&salt, &storedKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.ErrInvalidCredentials
	}
	if err != nil {
		return err
	}
	oldKey, err := auth.PasswordKey(oldPassword, salt)
	if err != nil || subtle.ConstantTimeCompare(oldKey, storedKey) != 1 {
		return auth.ErrInvalidCredentials
	}
	newSalt := make([]byte, 16)
	if _, err := rand.Read(newSalt); err != nil {
		return err
	}
	newKey, err := auth.PasswordKey(newPassword, newSalt)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx,
		`UPDATE users SET password_salt=$2, password_hash=$3 WHERE id=$1`,
		id, newSalt, newKey,
	)
	return err
}

func (s *Store) Users(ctx context.Context) ([]auth.User, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, email, account, language FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[auth.User])
}

func (s *Store) Names(ctx context.Context, ids []int64) (map[int64]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	names := make(map[int64]string, len(ids))
	if len(ids) == 0 {
		return names, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id, account FROM users WHERE id = ANY($1::bigint[])`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		names[id] = name
	}
	return names, rows.Err()
}

func isUniqueViolation(err error) bool {
	var pgError *pgconn.PgError
	return errors.As(err, &pgError) && pgError.Code == "23505"
}
