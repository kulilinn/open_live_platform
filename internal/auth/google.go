package auth

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var (
	ErrGoogleToken        = errors.New("invalid Google access token")
	ErrGoogleMailboxProof = errors.New("email verification code required to link this address")
	ErrGoogleUnavailable  = errors.New("Google sign-in is unavailable")
)

type GoogleConfig struct {
	ClientID string
	Client   *http.Client
}
type GoogleLogin struct {
	Config   GoogleConfig
	Accounts AccountStore
	Backend  JWTBackend
	Mail     *EmailVerification
}
type googleToken struct {
	Subject  string `json:"user_id"`
	Email    string `json:"email"`
	Verified bool   `json:"verified_email"`
	Audience string `json:"audience"`
	Expires  int64  `json:"expires_in"`
}

func (c GoogleConfig) get(ctx context.Context, address, bearer string, output any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return ErrGoogleUnavailable
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	res, err := client.Do(req)
	if err != nil {
		return ErrGoogleUnavailable
	}
	defer res.Body.Close()
	switch {
	case res.StatusCode == 400 || res.StatusCode == 401 || res.StatusCode == 403:
		return ErrGoogleToken
	case res.StatusCode != 200:
		return ErrGoogleUnavailable
	}
	if err = json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(output); err != nil {
		return ErrGoogleUnavailable
	}
	return nil
}

func (s GoogleLogin) Login(ctx context.Context, raw, code, language string) (Session, bool, error) {
	u, created, err := s.VerifyIdentity(ctx, raw, code, language, "")
	if err != nil {
		return Session{}, false, err
	}
	var session Session
	if accounts, ok := s.Accounts.(*JWTAccounts); ok {
		session, err = accounts.signInUser(ctx, u)
	} else {
		session, err = s.Backend.IssueVerifiedSession(ctx, u)
	}
	return session, created, err
}

func (s GoogleLogin) VerifyIdentity(ctx context.Context, raw, code, language, expectedEmail string) (User, bool, error) {
	if s.Config.ClientID == "" {
		return User{}, false, ErrGoogleUnavailable
	}
	if raw == "" {
		return User{}, false, ErrGoogleToken
	}
	var token googleToken
	if err := s.Config.get(ctx, "https://www.googleapis.com/oauth2/v2/tokeninfo?access_token="+url.QueryEscape(raw), "", &token); err != nil {
		return User{}, false, err
	}
	email, err := verificationEmail(token.Email)
	if err != nil || token.Subject == "" || token.Audience != s.Config.ClientID || !token.Verified || token.Expires <= 0 {
		return User{}, false, ErrGoogleToken
	}
	if expectedEmail != "" && NormalizeEmail(expectedEmail) != email {
		return User{}, false, ErrGoogleToken
	}
	authoritative := strings.HasSuffix(email, "@gmail.com")
	language = cmp.Or(language, "ja")
	id, created, err := s.Backend.ResolveGoogleUser(ctx, token.Subject, email, language, authoritative)
	if errors.Is(err, ErrGoogleMailboxProof) {
		// Google is authoritative for Workspace accounts with a hosted-domain claim.
		var profile struct{ ID, Email, HD string }
		if err = s.Config.get(ctx, "https://www.googleapis.com/oauth2/v2/userinfo", raw, &profile); err != nil {
			return User{}, false, err
		}
		if profile.ID != token.Subject || NormalizeEmail(profile.Email) != email {
			return User{}, false, ErrGoogleToken
		}
		if profile.HD == "" {
			if code == "" || s.Mail == nil {
				return User{}, false, ErrGoogleMailboxProof
			}
			if _, _, err = s.Mail.redeem(ctx, email, code, UseCodeForLogin); err != nil {
				return User{}, false, err
			}
		}
		id, created, err = s.Backend.ResolveGoogleUser(ctx, token.Subject, email, language, true)
	}
	if err != nil {
		return User{}, false, err
	}
	u, ok, err := s.Accounts.Find(ctx, id)
	if err != nil {
		return User{}, false, err
	}
	if !ok {
		return User{}, false, ErrGoogleToken
	}
	if u.Moderation.Banned() {
		return User{}, false, BannedError{Moderation: u.Moderation}
	}
	return u, created, nil
}

func (s *Store) ResolveGoogleUser(ctx context.Context, subject, email, language string, mayLink bool) (int64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	if id, ok := s.googleSubjects[subject]; ok {
		return id, false, nil
	}
	if !mayLink {
		return 0, false, ErrGoogleMailboxProof
	}
	login, exists := s.users[email]
	if exists {
		for _, id := range s.googleSubjects {
			if id == login.user.ID {
				return 0, false, ErrGoogleToken
			}
		}
	} else {
		s.nextID++
		login.user = User{ID: s.nextID, Email: email, Account: "User_" + rand.Text()[:10], Language: language}
		s.users[email] = login
		s.names[login.user.ID] = login.user.Account
	}
	if s.googleSubjects == nil {
		s.googleSubjects = make(map[string]int64)
	}
	s.googleSubjects[subject] = login.user.ID
	return login.user.ID, !exists, nil
}
