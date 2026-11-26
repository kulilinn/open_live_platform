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
	"time"
)

var (
	ErrMissingFields      = errors.New("email, password, and account are required")
	ErrEmailInUse         = errors.New("email is already registered")
	ErrInvalidCredentials = errors.New("invalid email or password")
)

type User struct {
	ID                  int64      `json:"id"`
	Email               string     `json:"email"`
	Account             string     `json:"account"`
	Language            string     `json:"language,omitempty"`
	TranslationLanguage string     `json:"-" db:"-"`
	TranslationEnabled  bool       `json:"-" db:"-"`
	Profile             *Profile   `json:"-" db:"-"`
	Moderation          Moderation `json:"-" db:"-"`
}

type Session struct {
	Token string `json:"token"`
	User  User   `json:"user"`
}

type ProvisionUser struct {
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"displayName"`
	IsAnchor    bool   `json:"isAnchor"`
}

type AccountStore interface {
	SignUp(ctx context.Context, email, password, account string) (Session, error)
	SignIn(ctx context.Context, email, password string) (Session, error)
	Lookup(ctx context.Context, token string) (User, bool, error)
	Bearer(ctx context.Context, authorization string) (User, bool, error)
	Refresh(ctx context.Context, token string) (Session, bool, error)
	Revoke(ctx context.Context, token string) error
	Find(ctx context.Context, id int64) (User, bool, error)
	FindBySearchName(ctx context.Context, name string) (int64, bool, error)
	SetLanguage(ctx context.Context, id int64, language string) error
	// TranslationLanguages returns stored preferences for existing accounts. An empty preference means the default.
	TranslationLanguages(ctx context.Context, ids []int64) (map[int64]string, error)
	SetTranslationLanguage(ctx context.Context, id int64, language string) error
	SetTranslationSettings(ctx context.Context, id int64, enabled *bool, language *string) error
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
	googleSubjects       map[string]int64
	mu                   sync.RWMutex
	nextID               int64
	users                map[string]credentials
	names                map[int64]string
	translationLanguages map[int64]string
	translationEnabled   map[int64]bool
	sessions             map[string]User
	jwtSessions          map[int64]int64
	sessionRevocations   SessionRevocations
	emailCodes           map[string]emailCode
	nextEmailCodeSweep   time.Time
	loginGrants          map[[32]byte]LoginGrant
	nextLoginGrantSweep  time.Time
}

var _ AccountStore = (*Store)(nil)

func NewStore() *Store {
	return &Store{
		users:                make(map[string]credentials),
		names:                make(map[int64]string),
		translationLanguages: make(map[int64]string),
		translationEnabled:   make(map[int64]bool),
		sessions:             make(map[string]User),
		jwtSessions:          make(map[int64]int64),
	}
}

func (s *Store) SignUp(ctx context.Context, email, password, account string) (Session, error) {
	if err := ctx.Err(); err != nil {
		return Session{}, err
	}
	email = NormalizeEmail(email)
	account = strings.TrimSpace(account)
	if email == "" || strings.ContainsRune(email, 0) || password == "" || account == "" {
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
	if login.user.Moderation.Banned() {
		return User{}, BannedError{Moderation: login.user.Moderation}
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
	if exists {
		user.Moderation = s.users[user.accountKey()].user.Moderation
	}
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
	user = s.users[user.accountKey()].user
	if user.Moderation.Banned() {
		return Session{}, false, BannedError{Moderation: user.Moderation}
	}
	return s.newSession(user), true, nil
}

func (s *Store) Revoke(ctx context.Context, token string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.sessions[token]; exists {
		delete(s.sessions, token)
		s.sessionRevocations.Revoke(OpaqueSessionKey(token))
	}
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
			login.user.TranslationLanguage = s.translationLanguages[id]
			login.user.TranslationEnabled = s.translationEnabled[id]
			return login.user, true, nil
		}
	}
	return User{}, false, nil
}
func (s *Store) FindBySearchName(ctx context.Context, name string) (int64, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var best int64
	for _, login := range s.users {
		u := login.user
		n := u.Account
		if u.Profile != nil {
			n = u.Profile.SearchName
		}
		if strings.EqualFold(name, n) && (best == 0 || u.ID < best) {
			best = u.ID
		}
	}
	return best, best != 0, ctx.Err()
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

func (s *Store) TranslationLanguages(ctx context.Context, ids []int64) (map[int64]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	languages := make(map[int64]string, len(ids))
	for _, id := range ids {
		if _, exists := s.names[id]; exists {
			languages[id] = s.translationLanguages[id]
		}
	}
	return languages, nil
}

func (s *Store) SetTranslationLanguage(ctx context.Context, id int64, language string) error {
	return s.SetTranslationSettings(ctx, id, nil, &language)
}

func (s *Store) SetTranslationSettings(ctx context.Context, id int64, enabled *bool, language *string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.names[id]; exists {
		if language != nil {
			s.translationLanguages[id] = *language
		}
		if enabled != nil {
			s.translationEnabled[id] = *enabled
		}
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
		if login.user.Moderation.Banned() {
			return BannedError{Moderation: login.user.Moderation}
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
