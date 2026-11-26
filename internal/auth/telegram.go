package auth

import (
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

var ErrTelegramCollision = errors.New("Telegram identity conflicts with an existing account")
var ErrTelegramLogin = errors.New("invalid Telegram login")

func (u User) accountKey() string {
	if u.Email == "" {
		return "\x00" + strconv.FormatInt(u.ID, 10)
	}
	return u.Email
}

type telegramUser struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

func SignInTelegram(ctx context.Context, accounts AccountStore, backend JWTBackend, token, initData, language string) (Session, error) {
	params, err := url.ParseQuery(initData)
	if err != nil || token == "" {
		return Session{}, ErrTelegramLogin
	}
	got, err := hex.DecodeString(params.Get("hash"))
	if err != nil {
		return Session{}, ErrTelegramLogin
	}
	params.Del("hash")
	fields := make([]string, 0, len(params))
	for key, v := range params {
		if len(v) != 1 {
			return Session{}, ErrTelegramLogin
		}
		fields = append(fields, key+"="+v[0])
	}
	slices.Sort(fields)
	derive := hmac.New(sha256.New, []byte("WebAppData"))
	derive.Write([]byte(token))
	mac := hmac.New(sha256.New, derive.Sum(nil))
	mac.Write([]byte(strings.Join(fields, "\n")))
	ts, err := strconv.ParseInt(params.Get("auth_date"), 10, 64)
	age := time.Since(time.Unix(ts, 0))
	var input telegramUser
	if !hmac.Equal(got, mac.Sum(nil)) || err != nil || age > 24*time.Hour || age < -time.Minute || json.Unmarshal([]byte(params.Get("user")), &input) != nil || input.ID < 1 || input.ID >= 1<<52 {
		return Session{}, ErrTelegramLogin
	}
	u, err := backend.ResolveTelegramUser(ctx, input.ID, cmp.Or(input.Username, input.FirstName), cmp.Or(language, "ja"))
	if err != nil {
		return Session{}, err
	}
	if u.Moderation.Banned() {
		return Session{}, BannedError{Moderation: u.Moderation}
	}
	if jwt, ok := accounts.(*JWTAccounts); ok {
		return jwt.signInUser(ctx, u)
	}
	return backend.IssueVerifiedSession(ctx, u)
}
func (s *Store) ResolveTelegramUser(ctx context.Context, id int64, name, language string) (User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return User{}, err
	}
	u := User{ID: id, Account: name, Language: language}
	key := u.accountKey()
	if entry, ok := s.users[key]; ok {
		return entry.user, nil
	}
	if _, ok := s.names[id]; ok {
		return User{}, ErrTelegramCollision
	}
	s.nextID = max(s.nextID, id)
	s.names[id] = name
	s.users[key] = credentials{user: u}
	return u, nil
}
