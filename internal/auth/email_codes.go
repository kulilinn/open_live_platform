package auth

import (
	"context"
	"crypto/rand"
	"time"
)

type emailCode struct {
	userID    int64
	digest    [32]byte
	expiresAt time.Time
	attempts  int
	delivered bool
	redeemed  bool
}

func (s *Store) FindByEmail(ctx context.Context, email string) (User, bool, error) {
	if err := ctx.Err(); err != nil {
		return User{}, false, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	login, found := s.users[email]
	return login.user, found, nil
}

func (s *Store) ReserveEmailCode(ctx context.Context, email string, userID int64, digest [32]byte, expiresAt time.Time) (EmailCodeOffer, error) {
	if err := ctx.Err(); err != nil {
		return EmailCodeOffer{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.emailCodes == nil {
		s.emailCodes = make(map[string]emailCode)
	}
	now := time.Now()
	if !now.Before(s.nextEmailCodeSweep) {
		for key, value := range s.emailCodes {
			if !now.Before(value.expiresAt) {
				delete(s.emailCodes, key)
			}
		}
		s.nextEmailCodeSweep = now.Add(time.Minute)
	}
	if prior, ok := s.emailCodes[email]; ok && prior.userID == userID && !prior.redeemed && now.Before(prior.expiresAt) {
		return EmailCodeOffer{ExpiresAt: prior.expiresAt, Delivered: prior.delivered}, nil
	}
	s.emailCodes[email] = emailCode{userID: userID, digest: digest, expiresAt: expiresAt}
	return EmailCodeOffer{ExpiresAt: expiresAt, New: true}, nil
}

func (s *Store) FinishEmailCode(ctx context.Context, email string, digest [32]byte, delivered bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	code, ok := s.emailCodes[email]
	if !ok || code.digest != digest || code.delivered {
		return ErrEmailCodeDelivery
	}
	if !delivered {
		delete(s.emailCodes, email)
		return nil
	}
	if !time.Now().Before(code.expiresAt) {
		delete(s.emailCodes, email)
		return ErrEmailCodeDelivery
	}
	code.delivered = true
	s.emailCodes[email] = code
	return nil
}

func (s *Store) RedeemEmailCode(ctx context.Context, email string, digest [32]byte, use EmailCodeUse) (int64, bool, error) {
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	code, ok := s.emailCodes[email]
	if !ok || !code.delivered || code.redeemed || code.attempts >= EmailCodeMaxAttempts || !time.Now().Before(code.expiresAt) ||
		(use == UseCodeForSignup && code.userID != 0) || (use == UseCodeForReset && code.userID == 0) {
		return 0, false, nil
	}
	code.attempts++
	code.redeemed = code.digest == digest
	s.emailCodes[email] = code
	return code.userID, code.redeemed, nil
}

func (s *Store) ResetVerifiedPassword(ctx context.Context, userID int64, email, password string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var salt [16]byte
	rand.Read(salt[:])
	key, err := PasswordKey(password, salt[:])
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	login, ok := s.users[email]
	if !ok || login.user.ID != userID {
		return ErrInvalidEmailCode
	}
	if login.user.Moderation.Banned() {
		return BannedError{Moderation: login.user.Moderation}
	}
	login.salt, login.key = salt, key
	s.users[email] = login
	return nil
}

func (s *Store) IssueVerifiedSession(ctx context.Context, user User) (Session, error) {
	if err := ctx.Err(); err != nil {
		return Session{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	login, ok := s.users[user.accountKey()]
	if !ok || login.user.ID != user.ID {
		return Session{}, ErrInvalidCredentials
	}
	if login.user.Moderation.Banned() {
		return Session{}, BannedError{Moderation: login.user.Moderation}
	}
	return s.newSession(login.user), nil
}
