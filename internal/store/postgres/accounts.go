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
		RETURNING id, email, account, language, profile`, email, account, salt, key).
		Scan(&user.ID, &user.Email, &user.Account, &user.Language, &user.Profile)
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
	user, err := s.VerifyPassword(ctx, email, password)
	if err != nil {
		return auth.Session{}, err
	}

	return s.issueOpaqueSession(ctx, user)
}

func (s *Store) issueOpaqueSession(ctx context.Context, user auth.User) (auth.Session, error) {
	token := rand.Text()
	tag, err := s.pool.Exec(ctx, `INSERT INTO sessions(token,user_id)
		SELECT $1,u.id FROM users u WHERE u.id=$2 AND COALESCE(u.email,'')=$3
		AND NOT (u.account_status=2 AND `+activeAccountLimit+`) FOR SHARE`, token, user.ID, user.Email)
	if err != nil {
		return auth.Session{}, err
	}
	if tag.RowsAffected() == 0 {
		return auth.Session{}, s.sessionIssueError(ctx, user.ID)
	}
	return auth.Session{Token: token, User: user}, nil
}

func (s *Store) sessionIssueError(ctx context.Context, userID int64) error {
	user, found, err := s.Find(ctx, userID)
	if err != nil {
		return err
	}
	if found && user.Moderation.Banned() {
		return auth.BannedError{Moderation: user.Moderation}
	}
	return auth.ErrInvalidCredentials
}

func (s *Store) VerifyPassword(ctx context.Context, email, password string) (auth.User, error) {
	var user auth.User
	var salt, storedKey []byte
	err := s.pool.QueryRow(ctx, `
		SELECT u.id, u.email, u.account, u.language, u.profile, u.password_salt, u.password_hash, `+accountModerationColumns+`
		FROM users u`+accountModerationJoin+`WHERE u.email=$1`, auth.NormalizeEmail(email)).
		Scan(&user.ID, &user.Email, &user.Account, &user.Language, &user.Profile, &salt, &storedKey, &user.Moderation.Type, &user.Moderation.Until, &user.Moderation.Total, &user.Moderation.Reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.User{}, auth.ErrInvalidCredentials
	}
	if err != nil {
		return auth.User{}, err
	}
	if !passwordMatches(password, salt, storedKey) {
		return auth.User{}, auth.ErrInvalidCredentials
	}
	if user.Moderation.Banned() {
		return auth.User{}, auth.BannedError{Moderation: user.Moderation}
	}
	return user, nil
}

func passwordMatches(password string, salt, storedKey []byte) bool {
	if len(salt) == 0 {
		legacy, ok := auth.ParseLegacyPassword(string(storedKey))
		return ok && legacy.Matches(password)
	}
	key, err := auth.PasswordKey(password, salt)
	return err == nil && subtle.ConstantTimeCompare(key, storedKey) == 1
}

func (s *Store) Lookup(ctx context.Context, token string) (auth.User, bool, error) {
	var user auth.User
	err := s.pool.QueryRow(ctx, `
		SELECT u.id, COALESCE(u.email,''), u.account, u.language, `+accountModerationColumns+`
		FROM sessions s JOIN users u ON u.id=s.user_id`+accountModerationJoin+`
		WHERE s.token = $1`, token).Scan(&user.ID, &user.Email, &user.Account, &user.Language,
		&user.Moderation.Type, &user.Moderation.Until, &user.Moderation.Total, &user.Moderation.Reason)
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
	user, found, err := s.Lookup(ctx, token)
	if err != nil || !found {
		return auth.Session{}, false, err
	}
	if user.Moderation.Banned() {
		return auth.Session{}, false, auth.BannedError{Moderation: user.Moderation}
	}
	session, err := s.issueOpaqueSession(ctx, user)
	return session, err == nil, err
}

func (s *Store) Revoke(ctx context.Context, token string) error {
	key := auth.OpaqueSessionKey(token)
	tag, err := s.pool.Exec(ctx, `
		WITH revoked AS (DELETE FROM sessions WHERE token=$1 RETURNING 1)
		SELECT pg_notify($2,$3) FROM revoked`, token, s.eventChannel, sessionRevocationPrefix+string(key))
	if err == nil && tag.RowsAffected() != 0 {
		s.sessionRevocations.Revoke(key)
	}
	return err
}

func (s *Store) Find(ctx context.Context, id int64) (auth.User, bool, error) {
	var user auth.User
	err := s.pool.QueryRow(ctx,
		`SELECT u.id,COALESCE(u.email,''),u.account,u.language,u.trans_lang,u.trans_enabled,u.profile,`+accountModerationColumns+
			` FROM users u`+accountModerationJoin+`WHERE u.id=$1`, id,
	).Scan(&user.ID, &user.Email, &user.Account, &user.Language, &user.TranslationLanguage, &user.TranslationEnabled, &user.Profile,
		&user.Moderation.Type, &user.Moderation.Until, &user.Moderation.Total, &user.Moderation.Reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.User{}, false, nil
	}
	return user, err == nil, err
}
func (s *Store) FindBySearchName(ctx context.Context, name string) (int64, bool, error) {
	var id int64
	err := s.pool.QueryRow(ctx, "SELECT id FROM users WHERE lower(coalesce(profile->>'SearchName',account))=lower($1) ORDER BY id LIMIT 1", name).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	return id, err == nil, err
}

func (s *Store) SetLanguage(ctx context.Context, id int64, language string) error {
	_, err := s.pool.Exec(ctx, `UPDATE users SET language = $2 WHERE id = $1`, id, language)
	return err
}

func (s *Store) TranslationLanguages(ctx context.Context, ids []int64) (map[int64]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	languages := make(map[int64]string, len(ids))
	if len(ids) == 0 {
		return languages, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id, trans_lang FROM users WHERE id = ANY($1::bigint[])`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var language string
		if err := rows.Scan(&id, &language); err != nil {
			return nil, err
		}
		languages[id] = language
	}
	return languages, rows.Err()
}

func (s *Store) SetTranslationLanguage(ctx context.Context, id int64, language string) error {
	return s.SetTranslationSettings(ctx, id, nil, &language)
}

func (s *Store) SetTranslationSettings(ctx context.Context, id int64, enabled *bool, language *string) error {
	_, err := s.pool.Exec(ctx, `UPDATE users SET trans_lang=COALESCE($2,trans_lang), trans_enabled=COALESCE($3,trans_enabled) WHERE id=$1`, id, language, enabled)
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
	if !passwordMatches(oldPassword, salt, storedKey) {
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
	tag, err := s.pool.Exec(ctx,
		`UPDATE users u SET password_salt=$2, password_hash=$3
		 WHERE id=$1 AND password_salt=$4 AND password_hash=$5 AND NOT (u.account_status=2 AND `+activeAccountLimit+`)`,
		id, newSalt, newKey, salt, storedKey,
	)
	if err == nil && tag.RowsAffected() == 0 {
		return auth.ErrInvalidCredentials
	}
	return err
}

func (s *Store) Users(ctx context.Context) ([]auth.User, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, COALESCE(email,''), account, language FROM users ORDER BY id`)
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
