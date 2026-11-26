package date

import (
	"context"
	"errors"
	"math"
	"time"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/payment"
	"open_live_platform/internal/subscription"
)

var (
	ErrAdvanced = errors.New("advanced subscription required")
	ErrPricing  = errors.New("invalid date rate")
)

type Service struct {
	Store         Store
	accounts      auth.AccountStore
	subscriptions subscription.Store
	billing       *payment.Client
}

func New(store Store, accounts auth.AccountStore, subscriptions subscription.Store, billing *payment.Client) *Service {
	return &Service{Store: store, accounts: accounts, subscriptions: subscriptions, billing: billing}
}

func (s *Service) Start(ctx context.Context, actor int64) error {
	user, ok, err := s.accounts.Find(ctx, actor)
	if err != nil {
		return err
	}
	if !ok || user.Profile == nil || !user.Profile.IsAnchor || user.Moderation.Banned() {
		return ErrForbidden
	}
	return s.Store.ChangeDateHost(ctx, actor, func(host *Host, current *Session, now time.Time) error {
		if current.Active(now) {
			return ErrBusy
		}
		End(host, current, now)
		if host.Channel == 0 || !host.Accepting {
			host.Channel = NewChannel()
		}
		host.Accepting = true
		host.AcceptUntil = now.Add(AcceptingLease)
		host.RecommendedAt = now
		host.Pending = nil
		return nil
	})
}

func (s *Service) RefreshOnline(ctx context.Context, anchor int64) error {
	return s.Store.ChangeDateHost(ctx, anchor, func(host *Host, session *Session, now time.Time) error {
		if host.Accepting && !host.Pending.Active(now) && !session.Active(now) {
			host.AcceptUntil = now.Add(AcceptingLease)
		}
		return nil
	})
}

func (s *Service) CallIn(ctx context.Context, actor, anchor int64, candidate Invitation) (Invitation, error) {
	if actor == anchor {
		return Invitation{}, ErrSelf
	}
	if err := auth.CheckCommunication(ctx, s.accounts, actor); err != nil {
		return Invitation{}, err
	}
	host, _, err := s.Store.DateHost(ctx, anchor)
	if err != nil || (!host.Listening(time.Now()) && !host.Pending.Active(time.Now())) {
		if err != nil {
			return Invitation{}, err
		}
		return Invitation{}, ErrNotListening
	}
	if err := s.checkEligibility(ctx, actor, anchor, host); err != nil {
		return Invitation{}, err
	}
	err = s.Store.ChangeDateHost(ctx, anchor, func(current *Host, session *Session, now time.Time) error {
		if (!current.Listening(now) && !current.Pending.Active(now)) || current.Channel != host.Channel || current.PricePerMin != host.PricePerMin || current.InitialMins != host.InitialMins {
			return ErrNotListening
		}
		if session.Active(now) {
			return ErrBusy
		}
		End(current, session, now)
		for uid, until := range current.Cooldowns {
			if !until.After(now) {
				delete(current.Cooldowns, uid)
			}
		}
		if current.Cooldowns[actor].After(now) {
			return ErrCooldown
		}
		if current.Pending.Active(now) {
			if current.Pending.UID != actor {
				return ErrBusy
			}
			candidate = *current.Pending
			return nil
		}
		candidate.Timestamp = now.Unix()
		current.Pending = &candidate
		return nil
	})
	return candidate, err
}

func (s *Service) SetLanguage(ctx context.Context, known Session, actor int64, language string) error {
	return s.Store.ChangeDateHost(ctx, known.AnchorID, func(_ *Host, session *Session, now time.Time) error {
		if session.Channel != known.Channel || !session.Active(now) || !session.Includes(actor) {
			return ErrClosed
		}
		if actor == session.AnchorID {
			session.AnchorLanguage = language
		} else {
			session.AudienceLanguage = language
		}
		return nil
	})
}

func (s *Service) CancelInvitation(ctx context.Context, actor, anchor int64) (int64, error) {
	var audience int64
	err := s.Store.ChangeDateHost(ctx, anchor, func(host *Host, _ *Session, now time.Time) error {
		if !host.Pending.Active(now) {
			host.Pending = nil
			return nil
		}
		audience = host.Pending.UID
		if actor != anchor && actor != audience {
			return ErrForbidden
		}
		if host.Cooldowns == nil {
			host.Cooldowns = make(map[int64]time.Time)
		}
		host.Cooldowns[audience] = now.Add(CallCooldown)
		host.Pending = nil
		if host.Accepting {
			host.AcceptUntil = now.Add(AcceptingLease)
		}
		return nil
	})
	return audience, err
}

func (s *Service) CallOut(ctx context.Context, anchor, audience int64, language string) (Session, error) {
	if err := auth.CheckCommunication(ctx, s.accounts, anchor); err != nil {
		return Session{}, err
	}
	host, current, err := s.Store.DateHost(ctx, anchor)
	if err != nil {
		return Session{}, err
	}
	if current.Active(time.Now()) && current.AudienceID == audience {
		return current, nil
	}
	if !host.Accepting || host.Pending == nil || host.Pending.UID != audience {
		return Session{}, ErrNoInvitation
	}
	if err := s.checkEligibility(ctx, audience, anchor, host); err != nil {
		return Session{}, err
	}
	var result Session
	err = s.Store.ChangeDateHost(ctx, anchor, func(current *Host, session *Session, now time.Time) error {
		if session.Active(now) && session.AudienceID == audience {
			result = *session
			return nil
		}
		if !current.Accepting || current.Channel != host.Channel || !current.Pending.Active(now) || current.Pending.UID != audience ||
			current.PricePerMin != host.PricePerMin || current.InitialMins != host.InitialMins || (session.Channel != 0 && session.EndedAt.IsZero()) {
			return ErrNoInvitation
		}
		result = Session{Channel: current.Channel, AnchorID: anchor, AudienceID: audience, StartedAt: now, AnchorSeen: now, AudienceSeen: now,
			AnchorLanguage: language, AudienceLanguage: current.Pending.Language, PricePerMin: current.PricePerMin, Points: current.PricePerMin * current.InitialMins}
		*session = result
		current.Pending = nil
		return nil
	})
	return result, err
}

func (s *Service) Tip(ctx context.Context, actor, anchor, points int64) (Invitation, error) {
	var info Invitation
	err := s.Store.ChangeDateHost(ctx, anchor, func(host *Host, _ *Session, now time.Time) error {
		if !host.Pending.Active(now) || host.Pending.UID != actor {
			return ErrNoInvitation
		}
		if points <= 0 || points > math.MaxInt64-host.Pending.Tips {
			return ErrPricing
		}
		host.Pending.Tips += points
		info = *host.Pending
		return nil
	})
	return info, err
}

func (s *Service) SessionFor(ctx context.Context, channel, actor int64, requireActive bool) (Session, error) {
	session, err := s.Store.DateSession(ctx, channel)
	if err != nil {
		return Session{}, err
	}
	if !session.Includes(actor) {
		return Session{}, ErrForbidden
	}
	if requireActive && !session.Active(time.Now()) {
		return Session{}, ErrClosed
	}
	return session, nil
}

func (s *Service) Heartbeat(ctx context.Context, channel, actor int64) (Session, bool, error) {
	known, err := s.SessionFor(ctx, channel, actor, true)
	if err != nil {
		return Session{}, false, err
	}
	var balance int64 = math.MaxInt64
	if actor == known.AudienceID && known.PricePerMin > 0 && sessionCost(known, time.Now()) > known.Points {
		balance, err = s.billing.Balance(ctx, actor)
		if err != nil {
			return Session{}, false, err
		}
	}
	var result Session
	var joined, closed, exhausted bool
	err = s.Store.ChangeDateHost(ctx, known.AnchorID, func(host *Host, session *Session, now time.Time) error {
		if session.Channel != channel || !session.Includes(actor) {
			return ErrClosed
		}
		if !session.Active(now) {
			End(host, session, now)
			closed = true
		} else if actor == session.AnchorID {
			joined = !session.AnchorJoined
			session.AnchorJoined, session.AnchorSeen = true, now
		} else {
			cost := sessionCost(*session, now)
			if cost-session.Points > balance {
				End(host, session, now)
				exhausted = true
			} else {
				session.Points = max(session.Points, cost)
				joined = !session.AudienceJoined
				session.AudienceJoined, session.AudienceSeen = true, now
			}
		}
		result = *session
		return nil
	})
	if err == nil {
		switch {
		case exhausted:
			err = payment.ErrInsufficientPoints
		case closed:
			err = ErrClosed
		}
	}
	return result, joined, err
}

func (s *Service) Leave(ctx context.Context, channel, actor int64) (Session, bool, error) {
	known, err := s.SessionFor(ctx, channel, actor, false)
	if err != nil {
		return Session{}, false, err
	}
	var ended bool
	err = s.Store.ChangeDateHost(ctx, known.AnchorID, func(host *Host, session *Session, now time.Time) error {
		if session.Channel == channel && session.Includes(actor) && session.EndedAt.IsZero() {
			End(host, session, now)
			known, ended = *session, true
		}
		return nil
	})
	return known, ended, err
}

func (s *Service) Stop(ctx context.Context, actor int64) (Session, int64, error) {
	var ended Session
	var invitee int64
	err := s.Store.ChangeDateHost(ctx, actor, func(host *Host, session *Session, now time.Time) error {
		if host.Pending.Active(now) {
			invitee = host.Pending.UID
		}
		host.Accepting, host.Pending = false, nil
		if session.Channel != 0 && session.EndedAt.IsZero() {
			End(host, session, now)
			ended = *session
		}
		return nil
	})
	return ended, invitee, err
}

func (s *Service) Sweep(ctx context.Context, onEnd func(Session), onExpire func(int64, int64)) error {
	anchors, err := s.Store.ActiveDateHosts(ctx)
	if err != nil {
		return err
	}
	var sweepErr error
	for _, anchor := range anchors {
		var ended Session
		var expired int64
		err := s.Store.ChangeDateHost(ctx, anchor, func(host *Host, session *Session, now time.Time) error {
			if host.Pending != nil && !host.Pending.Active(now) {
				expired = host.Pending.UID
				host.Pending = nil
				if host.Accepting {
					host.AcceptUntil = now.Add(AcceptingLease)
				}
			}
			if session.Channel != 0 && session.EndedAt.IsZero() && !session.Active(now) {
				End(host, session, now)
				ended = *session
			}
			return nil
		})
		sweepErr = errors.Join(sweepErr, err)
		if err == nil && ended.Channel != 0 {
			onEnd(ended)
		}
		if err == nil && expired != 0 {
			onExpire(anchor, expired)
		}
	}
	return sweepErr
}

func (s *Service) checkEligibility(ctx context.Context, fan, anchor int64, host Host) error {
	entries, err := s.subscriptions.Relationships(ctx, fan, anchor)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Level+subscription.Free == subscription.Advanced && entry.Active(time.Now()) {
			if required := host.PricePerMin * host.InitialMins; required > 0 {
				balance, err := s.billing.Balance(ctx, fan)
				if err != nil {
					return err
				}
				if balance < required {
					return payment.ErrInsufficientPoints
				}
			}
			return nil
		}
	}
	return ErrAdvanced
}

func sessionCost(session Session, now time.Time) int64 {
	minutes := (session.Seconds(now) + 59) / 60
	if session.PricePerMin > 0 && minutes > math.MaxInt64/session.PricePerMin {
		return math.MaxInt64
	}
	return minutes * session.PricePerMin
}
