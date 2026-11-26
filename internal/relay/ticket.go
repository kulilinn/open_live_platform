package relay

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/transport/httpx"
)

type RustOptions struct {
	AppID     int64
	AppKey    string
	AppSecret string
	AppUsers  []int64
	TTL       time.Duration
	Tickets   RustTicketStore
	Devices   RustDeviceStore
	Grants    RustControlStore
}

type RustTicketRecord struct {
	Token     string
	LoginKey  auth.SessionKey
	UserID    int64
	Login     auth.RelaySession
	Debug     bool
	IssuedAt  int64
	ExpiresAt time.Time
}

type RustTicketStore interface {
	IssueRustRelayTicket(context.Context, string, RustTicketRecord, time.Duration) (RustTicketRecord, error)
	LookupRustRelayTicket(context.Context, string, string) (RustTicketRecord, bool, error)
	RenewRustRelayTicket(context.Context, string, string, time.Duration) error
}

type rustTicket struct {
	RustTicketRecord
	signature string
}

type rustTickets struct {
	appID          int64
	appKey, secret string
	appUsers       map[int64]struct{}
	ttl            time.Duration
	mu             sync.Mutex
	byToken        map[string]*rustTicket
	bySession      map[auth.SessionKey]*rustTicket
	store          RustTicketStore
}

func newRustTickets(options RustOptions) *rustTickets {
	if options.TTL <= 0 {
		options.TTL = 24 * time.Hour
	}
	users := make(map[int64]struct{}, len(options.AppUsers))
	for _, id := range options.AppUsers {
		users[id] = struct{}{}
	}
	return &rustTickets{appID: options.AppID, appKey: options.AppKey, secret: options.AppSecret, appUsers: users, ttl: options.TTL, store: options.Tickets,
		byToken: make(map[string]*rustTicket), bySession: make(map[auth.SessionKey]*rustTicket)}
}

func rustSignature(appKey, secret, message string) string {
	mac := hmac.New(sha1.New, []byte(appKey+secret))
	mac.Write([]byte(message))
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *rustTickets) issue(ctx context.Context, login auth.RelaySession, uid int64, now time.Time) (*rustTicket, error) {
	return s.issueTicket(ctx, login, uid, now, true, false)
}

func (s *rustTickets) issueTicket(ctx context.Context, login auth.RelaySession, uid int64, now time.Time, reusable, debug bool) (*rustTicket, error) {
	var random [16]byte
	rand.Read(random[:])
	token := hex.EncodeToString(random[:])
	key := login.Key()
	if !reusable {
		key = auth.SessionKey("relay-acquire:" + token)
	}
	record := RustTicketRecord{Token: token, LoginKey: key, UserID: uid, Login: login, Debug: debug, IssuedAt: now.Unix(), ExpiresAt: now.Add(s.ttl)}
	if s.store != nil {
		record, err := s.store.IssueRustRelayTicket(ctx, s.appKey, record, s.ttl)
		if err != nil {
			return nil, err
		}
		return s.sign(record), nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ticket := s.bySession[key]; ticket != nil && now.Before(ticket.ExpiresAt) {
		return ticket, nil
	}
	ticket := s.sign(record)
	s.byToken[record.Token] = ticket
	s.bySession[key] = ticket
	time.AfterFunc(s.ttl, func() { s.removeExpired(ticket) })
	return ticket, nil
}

func (s *rustTickets) sign(record RustTicketRecord) *rustTicket {
	message := record.Token + "&" + strconv.FormatInt(record.IssuedAt, 10) + "&" + strconv.FormatInt(record.UserID, 10)
	return &rustTicket{RustTicketRecord: record, signature: rustSignature(s.appKey, s.secret, message)}
}

func (s *rustTickets) verify(ctx context.Context, token, signature string, now time.Time) (*rustTicket, error) {
	if len(token) != 32 || len(signature) != 40 {
		return nil, nil
	}
	if s.store != nil {
		record, found, err := s.store.LookupRustRelayTicket(ctx, s.appKey, token)
		if err != nil || !found {
			return nil, err
		}
		ticket := s.sign(record)
		if !hmac.Equal([]byte(ticket.signature), []byte(signature)) {
			return nil, nil
		}
		return ticket, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ticket := s.byToken[token]
	if ticket == nil || !now.Before(ticket.ExpiresAt) || !hmac.Equal([]byte(ticket.signature), []byte(signature)) {
		return nil, nil
	}
	return ticket, nil
}

func (s *rustTickets) refresh(ctx context.Context, ticket *rustTicket, now time.Time) {
	if s.store != nil {
		_ = s.store.RenewRustRelayTicket(ctx, s.appKey, ticket.Token, s.ttl)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byToken[ticket.Token] == ticket && now.Before(ticket.ExpiresAt) {
		ticket.ExpiresAt = now.Add(s.ttl)
	}
}

func (s *rustTickets) removeExpired(ticket *rustTicket) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if delay := time.Until(ticket.ExpiresAt); delay > 0 {
		time.AfterFunc(delay, func() { s.removeExpired(ticket) })
		return
	}
	delete(s.byToken, ticket.Token)
	if s.bySession[ticket.LoginKey] == ticket {
		delete(s.bySession, ticket.LoginKey)
	}
}

func (s *rustTickets) loginUser(ctx context.Context, accounts auth.AccountStore, ticket *rustTicket) (auth.User, bool, error) {
	if ticket.Login.AppUser {
		if _, ok := s.appUsers[ticket.UserID]; !ok {
			return auth.User{}, false, nil
		}
	}
	user, ok, err := ticket.Login.Lookup(ctx, accounts)
	return user, ok && user.ID == ticket.UserID && !user.Moderation.Banned(), err
}

func connectorURL(r *http.Request, publicURL, token, signature string) string {
	base := strings.TrimRight(publicURL, "/")
	switch {
	case base == "":
		scheme := "ws"
		if r.TLS != nil {
			scheme = "wss"
		}
		base = scheme + "://" + r.Host
	case strings.HasPrefix(base, "http://"), strings.HasPrefix(base, "https://"):
		base = "ws" + strings.TrimPrefix(base, "http")
	}
	return base + "/api/connector/" + token + "/" + signature
}

func (s *rustTickets) verifyHTTP(accounts auth.AccountStore, w http.ResponseWriter, r *http.Request) {
	ticket, err := s.verify(r.Context(), r.PathValue("token"), r.PathValue("sig"), time.Now())
	if err != nil {
		httpx.Error(w, http.StatusServiceUnavailable, "relay tickets unavailable")
		return
	}
	if ticket != nil {
		user, ok, err := s.loginUser(r.Context(), accounts, ticket)
		if err != nil {
			httpx.Error(w, http.StatusInternalServerError, "authentication failed")
			return
		}
		if ok {
			httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": map[string]any{"app_key": s.appKey, "uid": user.ID}})
			return
		}
	}
	httpx.Error(w, http.StatusForbidden, "invalid or expired relay ticket")
}
