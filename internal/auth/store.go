package auth

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"sort"
	"strings"
	"sync"
)

var (
	ErrMissingFields      = errors.New("email, password, and account are required")
	ErrEmailInUse         = errors.New("email is already registered")
	ErrInvalidCredentials = errors.New("invalid email or password")
)

type User struct {
	ID       int64  `json:"id"`
	Email    string `json:"email"`
	Account  string `json:"account"`
	Language string `json:"language,omitempty"`
}

type Session struct {
	Token string `json:"token"`
	User  User   `json:"user"`
}

type AccountStore interface {
	SignUp(ctx context.Context, email, password, account string) (Session, error)
	SignIn(ctx context.Context, email, password string) (Session, error)
	Lookup(ctx context.Context, token string) (User, bool, error)
	Bearer(ctx context.Context, authorization string) (User, bool, error)
	Refresh(ctx context.Context, token string) (Session, bool, error)
	Revoke(ctx context.Context, token string) error
	Find(ctx context.Context, id int64) (User, bool, error)
	SetLanguage(ctx context.Context, id int64, language string) error
	ChangePassword(ctx context.Context, id int64, oldPassword, newPassword string) error
	Users(ctx context.Context) ([]User, error)
	Names(ctx context.Context, ids []int64) (map[int64]string, error)
}

type credentials struct {
	user User
	salt [16]byte
	key  []byte
}

type Store struct {
	mu          sync.RWMutex
	nextID      int64
	users       map[string]credentials
	names       map[int64]string
	sessions    map[string]User
	jwtSessions map[int64]int64
}

var _ AccountStore = (*Store)(nil)

func NewStore() *Store {
	return &Store{
		users:       make(map[string]credentials),
		names:       make(map[int64]string),
		sessions:    make(map[string]User),
		jwtSessions: make(map[int64]int64),
	}
}

func (s *Store) SignUp(ctx context.Context, email, password, account string) (Session, error) {
	if err := ctx.Err(); err != nil {
		return Session{}, err
	}
	email = NormalizeEmail(email)
	account = strings.TrimSpace(account)
	if email == "" || password == "" || account == "" {
		return Session{}, ErrMissingFields
	}

	var salt [16]byte
	rand.Read(salt[:])
	key, err := PasswordKey(password, salt[:])
	if err != nil {
		return Session{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.users[email]; exists {
		return Session{}, ErrEmailInUse
	}
	s.nextID++
	user := User{ID: s.nextID, Email: email, Account: account, Language: "ja"}
	s.users[email] = credentials{user: user, salt: salt, key: key}
	s.names[user.ID] = user.Account
	return s.newSession(user), nil
}

func (s *Store) SignIn(ctx context.Context, email, password string) (Session, error) {
	user, err := s.VerifyPassword(ctx, email, password)
	if err != nil {
		return Session{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.newSession(s.users[user.Email].user), nil
}

func (s *Store) VerifyPassword(ctx context.Context, email, password string) (User, error) {
	if err := ctx.Err(); err != nil {
		return User{}, err
	}
	s.mu.RLock()
	login, exists := s.users[NormalizeEmail(email)]
	s.mu.RUnlock()
	if !exists {
		return User{}, ErrInvalidCredentials
	}
	key, err := PasswordKey(password, login.salt[:])
	if err != nil {
		return User{}, err
	}
	if subtle.ConstantTimeCompare(key, login.key) != 1 {
		return User{}, ErrInvalidCredentials
	}
	return login.user, nil
}

func (s *Store) Lookup(ctx context.Context, token string) (User, bool, error) {
	if err := ctx.Err(); err != nil {
		return User{}, false, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	user, exists := s.sessions[token]
	return user, exists, nil
}

func (s *Store) Bearer(ctx context.Context, authorization string) (User, bool, error) {
	if err := ctx.Err(); err != nil {
		return User{}, false, err
	}
	token, found := strings.CutPrefix(authorization, "Bearer ")
	if !found {
		return User{}, false, nil
	}
	return s.Lookup(ctx, token)
}

func (s *Store) Refresh(ctx context.Context, token string) (Session, bool, error) {
	if err := ctx.Err(); err != nil {
		return Session{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	user, ok := s.sessions[token]
	if !ok {
		return Session{}, false, nil
	}
	return s.newSession(user), true, nil
}

func (s *Store) Revoke(ctx context.Context, token string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.sessions, token)
	s.mu.Unlock()
	return nil
}

func (s *Store) Find(ctx context.Context, id int64) (User, bool, error) {
	if err := ctx.Err(); err != nil {
		return User{}, false, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, login := range s.users {
		if login.user.ID == id {
			return login.user, true, nil
		}
	}
	return User{}, false, nil
}

func (s *Store) SetLanguage(ctx context.Context, id int64, language string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for email, login := range s.users {
		if login.user.ID != id {
			continue
		}
		login.user.Language = language
		s.users[email] = login
		for token, user := range s.sessions {
			if user.ID == id {
				user.Language = language
				s.sessions[token] = user
			}
		}
		return nil
	}
	return nil
}

func (s *Store) ChangePassword(ctx context.Context, id int64, oldPassword, newPassword string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if oldPassword == "" || newPassword == "" {
		return ErrMissingFields
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for email, login := range s.users {
		if login.user.ID != id {
			continue
		}
		oldKey, err := PasswordKey(oldPassword, login.salt[:])
		if err != nil || subtle.ConstantTimeCompare(oldKey, login.key) != 1 {
			return ErrInvalidCredentials
		}
		var salt [16]byte
		if _, err := rand.Read(salt[:]); err != nil {
			return err
		}
		key, err := PasswordKey(newPassword, salt[:])
		if err != nil {
			return err
		}
		login.salt, login.key = salt, key
		s.users[email] = login
		return nil
	}
	return ErrInvalidCredentials
}

func (s *Store) Users(ctx context.Context) ([]User, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	s.mu.RLock()
	users := make([]User, 0, len(s.users))
	for _, login := range s.users {
		users = append(users, login.user)
	}
	s.mu.RUnlock()
	sort.Slice(users, func(i, j int) bool { return users[i].ID < users[j].ID })
	return users, nil
}

func (s *Store) Names(ctx context.Context, ids []int64) (map[int64]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make(map[int64]string, len(ids))
	for _, id := range ids {
		if name, exists := s.names[id]; exists {
			names[id] = name
		}
	}
	return names, nil
}

func (s *Store) newSession(user User) Session {
	token := rand.Text()
	s.sessions[token] = user
	return Session{Token: token, User: user}
}

func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func PasswordKey(password string, salt []byte) ([]byte, error) {
	return pbkdf2.Key(sha256.New, password, salt, 600_000, 32)
}
