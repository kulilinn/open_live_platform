package auth

import (
	"context"
	"errors"
	"strings"
	"time"
)

const (
	AccountNormal = 0
	AccountMuted  = 1
	AccountBanned = 2
)

var ErrCommunicationRestricted = errors.New("account cannot send messages")

type Moderation struct {
	Type   int
	Until  int64
	Total  int64
	Reason string
}

func (m Moderation) ActiveAt(now time.Time) bool {
	return m.Type != AccountNormal && (m.Until == -1 || (m.Until > 0 && m.Until >= now.Unix()))
}

func (m Moderation) Banned() bool {
	return m.Type == AccountBanned && m.ActiveAt(time.Now())
}

func (m Moderation) Reasons() []string {
	if m.Type == AccountNormal {
		return []string{}
	}
	return strings.Split(m.Reason, ",")
}

func (m Moderation) Status() Moderation {
	if m.Type == AccountNormal {
		return Moderation{}
	}
	return m
}

type BannedError struct{ Moderation Moderation }

func (BannedError) Error() string { return "account is disabled" }

func CheckCommunication(ctx context.Context, accounts AccountStore, userID int64) error {
	user, found, err := accounts.Find(ctx, userID)
	if err != nil {
		return err
	}
	if !found || user.Moderation.ActiveAt(time.Now()) {
		return ErrCommunicationRestricted
	}
	return nil
}
