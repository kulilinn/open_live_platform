package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"
	"net/mail"
	"time"
)

var (
	ErrInvalidEmail      = errors.New("invalid email address")
	ErrInvalidEmailCode  = errors.New("invalid or expired verification code")
	ErrEmailNotFound     = errors.New("account not found")
	ErrEmailCodeDelivery = errors.New("could not deliver verification code")
)

type VerificationSender interface {
	SendVerificationCode(ctx context.Context, address, code string, validity time.Duration) error
}

type EmailCodeUse uint8

const (
	UseCodeForLogin EmailCodeUse = iota
	UseCodeForSignup
	UseCodeForReset
	EmailCodeMaxAttempts = 5
)

type EmailCodeOffer struct {
	ExpiresAt time.Time
	New       bool
	Delivered bool
}

type emailCodeStore interface {
	FindByEmail(context.Context, string) (User, bool, error)
	ReserveEmailCode(ctx context.Context, email string, userID int64, digest [32]byte, expiresAt time.Time) (EmailCodeOffer, error)
	FinishEmailCode(ctx context.Context, email string, digest [32]byte, delivered bool) error
	RedeemEmailCode(ctx context.Context, email string, digest [32]byte, use EmailCodeUse) (userID int64, ok bool, err error)
	ResetVerifiedPassword(ctx context.Context, userID int64, email, password string) error
	IssueVerifiedSession(ctx context.Context, user User) (Session, error)
}

type EmailVerification struct {
	accounts AccountStore
	store    emailCodeStore
	sender   VerificationSender
	secret   []byte
	ttl      time.Duration
}

func NewEmailVerification(accounts AccountStore, sender VerificationSender, secret string, ttl time.Duration) (*EmailVerification, error) {
	if sender == nil {
		return nil, nil
	}
	if secret == "" {
		return nil, errors.New("MAIL_CODE_SECRET or JWT_SECRET is required for email verification")
	}
	backend := accounts
	if jwt, ok := accounts.(*JWTAccounts); ok {
		backend = jwt.JWTBackend
	}
	store, ok := backend.(emailCodeStore)
	if !ok {
		return nil, errors.New("account store does not support email verification")
	}
	if ttl <= 0 {
		ttl = time.Minute
	}
	return &EmailVerification{accounts: accounts, store: store, sender: sender, secret: []byte(secret), ttl: ttl}, nil
}

func (s *EmailVerification) Issue(ctx context.Context, email string, signin bool) (EmailCodeOffer, error) {
	email, err := verificationEmail(email)
	if err != nil {
		return EmailCodeOffer{}, err
	}
	user, found, err := s.store.FindByEmail(ctx, email)
	if err != nil {
		return EmailCodeOffer{}, err
	}
	if signin && !found {
		return EmailCodeOffer{}, ErrEmailNotFound
	}
	if !signin && found {
		return EmailCodeOffer{}, ErrEmailInUse
	}
	number, err := rand.Int(rand.Reader, big.NewInt(10_000))
	if err != nil {
		return EmailCodeOffer{}, err
	}
	code := fmt.Sprintf("%04d", number.Int64())
	digest := s.digest(email, code)
	offer, err := s.store.ReserveEmailCode(ctx, email, user.ID, digest, time.Now().Add(s.ttl))
	if err != nil {
		return EmailCodeOffer{}, err
	}
	if !offer.New {
		if !offer.Delivered {
			return EmailCodeOffer{}, ErrEmailCodeDelivery
		}
		return offer, nil
	}
	sendErr := s.sender.SendVerificationCode(ctx, email, code, s.ttl)
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	err = s.store.FinishEmailCode(finishCtx, email, digest, sendErr == nil)
	if sendErr != nil {
		return EmailCodeOffer{}, ErrEmailCodeDelivery
	}
	return offer, err
}

func (s *EmailVerification) SignUp(ctx context.Context, email, code, password, account string) (Session, error) {
	canonical, _, err := s.redeem(ctx, email, code, UseCodeForSignup)
	if err != nil {
		return Session{}, err
	}
	return s.accounts.SignUp(ctx, canonical, password, account)
}

func (s *EmailVerification) Login(ctx context.Context, email, code string) (Session, error) {
	canonical, userID, err := s.redeem(ctx, email, code, UseCodeForLogin)
	if err != nil {
		return Session{}, err
	}
	if userID == 0 {
		// The generated password is never exposed. This account initially signs in via email.
		session, err := s.accounts.SignUp(ctx, canonical, rand.Text(), "User_"+rand.Text()[:10])
		if err != nil {
			return Session{}, err
		}
		if accounts, ok := s.accounts.(*JWTAccounts); ok {
			if err := accounts.Revoke(ctx, session.Token); err != nil {
				return Session{}, err
			}
			return accounts.signInUser(ctx, session.User)
		}
		return session, nil
	}
	user, found, err := s.accounts.Find(ctx, userID)
	if err != nil {
		return Session{}, err
	}
	if !found || user.Email != canonical {
		return Session{}, ErrInvalidEmailCode
	}
	if user.Moderation.Banned() {
		return Session{}, BannedError{Moderation: user.Moderation}
	}
	if accounts, ok := s.accounts.(*JWTAccounts); ok {
		return accounts.signInUser(ctx, user)
	}
	return s.store.IssueVerifiedSession(ctx, user)
}

func (s *EmailVerification) Reset(ctx context.Context, email, code, password string) error {
	canonical, userID, err := s.redeem(ctx, email, code, UseCodeForReset)
	if err != nil {
		return err
	}
	user, found, err := s.accounts.Find(ctx, userID)
	if err != nil {
		return err
	}
	if !found || user.Email != canonical {
		return ErrInvalidEmailCode
	}
	if user.Moderation.Banned() {
		return BannedError{Moderation: user.Moderation}
	}
	return s.store.ResetVerifiedPassword(ctx, userID, canonical, password)
}

func (s *EmailVerification) redeem(ctx context.Context, email, code string, use EmailCodeUse) (string, int64, error) {
	email, err := verificationEmail(email)
	if err != nil {
		return "", 0, err
	}
	if len(code) != 4 {
		return "", 0, ErrInvalidEmailCode
	}
	userID, ok, err := s.store.RedeemEmailCode(ctx, email, s.digest(email, code), use)
	if err != nil {
		return "", 0, err
	}
	if !ok {
		return "", 0, ErrInvalidEmailCode
	}
	return email, userID, nil
}

func (s *EmailVerification) digest(email, code string) [32]byte {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte("email-code\x00" + email + "\x00" + code))
	var digest [32]byte
	mac.Sum(digest[:0])
	return digest
}

func verificationEmail(email string) (string, error) {
	email = NormalizeEmail(email)
	address, err := mail.ParseAddress(email)
	if err != nil || address.Address != email || len(email) > 254 {
		return "", ErrInvalidEmail
	}
	return email, nil
}
