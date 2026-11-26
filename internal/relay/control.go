package relay

import (
	"context"
	"crypto/hmac"
	"net/http"
	"strconv"
	"strings"
	"time"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/transport/httpx"
)

type RustControlGrant struct {
	ControlledID int64
	Duplex       bool
}

type RustControlStore interface {
	SetRustControlGrant(context.Context, string, int64, RustControlGrant, time.Duration) error
	LookupRustControlGrant(context.Context, string, int64, int64) (RustControlGrant, bool, error)
}

type controlGrantKey struct{ controller, controlled int64 }

type controlGrantState struct {
	grant     RustControlGrant
	grantedAt time.Time
	expiresAt time.Time
}

func (s *rustTickets) registerControls(mux *http.ServeMux, accounts auth.AccountStore, devices *deviceHub, publicURL string) {
	if s.appID <= 0 {
		return
	}
	mux.HandleFunc("POST /api/connector/acquire", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			AppID      int64 `json:"appId,string"`
			InstanceID int64 `json:"instanceId,string"`
			Timestamp  string
			Signature  string
			IsStaff    bool `json:"isStaff"`
		}
		if !httpx.Decode(w, r, &input) {
			return
		}
		now := time.Now()
		ts, err := strconv.ParseInt(input.Timestamp, 10, 64)
		if err != nil || ts < now.Unix()-10 || ts > now.Unix()+10 {
			rustAppResponse(w, http.StatusOK, "4017", "invalid timestamp")
			return
		}
		if !s.checkAppID(w, input.AppID) {
			return
		}
		message := strconv.FormatInt(input.AppID, 10) + "&" + strconv.FormatInt(input.InstanceID, 10) + "&" + strconv.FormatInt(ts, 10)
		if !hmac.Equal([]byte(input.Signature), []byte(rustSignature(s.appKey, s.secret, message))) {
			rustForbidden(w)
			return
		}
		login, ok := s.authorizeAppUser(w, r, accounts, input.InstanceID, true)
		if !ok {
			return
		}
		ticket, err := s.issueTicket(r.Context(), login, input.InstanceID, now, false, input.IsStaff)
		if err != nil {
			rustAppResponse(w, http.StatusServiceUnavailable, "500", "relay tickets unavailable")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		httpx.JSON(w, http.StatusOK, map[string]string{"url": connectorURL(r, publicURL, ticket.Token, ticket.signature)})
	})
	mux.HandleFunc("POST /api/connector/vibrate", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			AppID      int64  `json:"appId,string"`
			InstanceID int64  `json:"instanceId,string"`
			AppSecret  string `json:"appSecret"`
			Payload    deviceCommand
		}
		if !httpx.Decode(w, r, &input) || !s.checkAppID(w, input.AppID) || !s.checkAppSecret(w, input.AppSecret) {
			return
		}
		if _, ok := s.authorizeAppUser(w, r, accounts, input.InstanceID, true); !ok {
			return
		}
		if input.Payload.Name != "vibrate" && input.Payload.Name != "mode_vibrate" {
			rustAppResponse(w, http.StatusOK, "500", "")
			return
		}
		if err := devices.emitControl(r.Context(), input.InstanceID, input.Payload); err != nil {
			rustAppResponse(w, http.StatusOK, "500", "")
			return
		}
		rustAppResponse(w, http.StatusOK, "200", "")
	})
	mux.HandleFunc("POST /api/connector/remote", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			AppID      int64  `json:"appId,string"`
			Controller int64  `json:"controller,string"`
			Controlled int64  `json:"controlled,string"`
			AppSecret  string `json:"appSecret"`
			Duplex     bool
			Expiry     *int64
		}
		if !httpx.Decode(w, r, &input) || !s.checkAppID(w, input.AppID) || !s.checkAppSecret(w, input.AppSecret) {
			return
		}
		if _, ok := s.authorizeAppUser(w, r, accounts, input.Controlled, false); !ok {
			return
		}
		if input.Controller <= 0 || input.Controller == input.Controlled || input.Expiry == nil || *input.Expiry < 0 || *input.Expiry > int64((1<<63-1)/time.Second) {
			rustAppResponse(w, http.StatusBadRequest, "400", "invalid control grant")
			return
		}
		if _, ok, err := accounts.Find(r.Context(), input.Controller); err != nil || !ok {
			rustAppResponse(w, http.StatusOK, "500", "")
			return
		}
		grant := RustControlGrant{ControlledID: input.Controlled, Duplex: input.Duplex}
		if err := devices.setGrant(r.Context(), input.Controller, grant, time.Duration(*input.Expiry)*time.Second); err != nil {
			rustAppResponse(w, http.StatusOK, "500", "")
			return
		}
		rustAppResponse(w, http.StatusOK, "200", "")
	})
}

func (s *rustTickets) checkAppID(w http.ResponseWriter, id int64) bool {
	if id != s.appID {
		rustAppResponse(w, http.StatusOK, "4011", "invalid app id")
		return false
	}
	return true
}

func (s *rustTickets) checkAppSecret(w http.ResponseWriter, secret string) bool {
	if !hmac.Equal([]byte(secret), []byte(s.secret)) {
		rustForbidden(w)
		return false
	}
	return true
}

func (s *rustTickets) authorizeAppUser(w http.ResponseWriter, r *http.Request, accounts auth.AccountStore, uid int64, allowEnrollment bool) (auth.RelaySession, bool) {
	if token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		user, found, err := accounts.Bearer(r.Context(), r.Header.Get("Authorization"))
		if err != nil {
			rustAppResponse(w, http.StatusServiceUnavailable, "500", "authentication unavailable")
			return auth.RelaySession{}, false
		}
		if login, valid := auth.RelaySessionForToken(accounts, token); found && user.ID == uid && valid && !user.Moderation.Banned() {
			return login, true
		}
	} else if _, enrolled := s.appUsers[uid]; allowEnrollment && enrolled {
		user, found, err := accounts.Find(r.Context(), uid)
		if err != nil {
			rustAppResponse(w, http.StatusServiceUnavailable, "500", "authentication unavailable")
			return auth.RelaySession{}, false
		}
		if found && !user.Moderation.Banned() {
			return auth.RelaySession{AppUser: true, UserID: uid}, true
		}
	}
	rustForbidden(w)
	return auth.RelaySession{}, false
}

func rustForbidden(w http.ResponseWriter) {
	rustAppResponse(w, http.StatusForbidden, "403", "forbidden")
}

func rustAppResponse(w http.ResponseWriter, code int, status, message string) {
	if message == "" {
		httpx.JSON(w, code, map[string]string{"status": status})
		return
	}
	httpx.JSON(w, code, map[string]string{"status": status, "message": message})
}

func (h *deviceHub) setGrant(ctx context.Context, controller int64, grant RustControlGrant, ttl time.Duration) error {
	if h.grants != nil {
		return h.grants.SetRustControlGrant(ctx, h.scope, controller, grant, ttl)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	key := controlGrantKey{controller, grant.ControlledID}
	if ttl == 0 {
		delete(h.memoryGrants, key)
		return nil
	}
	if h.memoryGrants == nil {
		h.memoryGrants = make(map[controlGrantKey]controlGrantState)
	}
	now := time.Now()
	h.memoryGrants[key] = controlGrantState{grant: grant, grantedAt: now, expiresAt: now.Add(ttl)}
	return nil
}

func (h *deviceHub) activeGrant(ctx context.Context, controller, requested int64) (RustControlGrant, bool, error) {
	if !h.controlsEnabled {
		return RustControlGrant{}, false, nil
	}
	if h.grants != nil {
		return h.grants.LookupRustControlGrant(ctx, h.scope, controller, requested)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	var latest controlGrantState
	for key, entry := range h.memoryGrants {
		if !now.Before(entry.expiresAt) {
			delete(h.memoryGrants, key)
		} else if key.controller == controller && (requested == 0 || key.controlled == requested) && entry.grantedAt.After(latest.grantedAt) {
			latest = entry
		}
	}
	return latest.grant, !latest.grantedAt.IsZero(), nil
}

func (h *deviceHub) routeControl(ctx context.Context, uid, requested int64, cmd deviceCommand) {
	if requested == uid {
		requested = 0
	}
	grant, ok, err := h.activeGrant(ctx, uid, requested)
	if err != nil || (!ok && requested != 0) {
		return
	}
	if !ok || grant.Duplex {
		_ = h.emitControl(ctx, uid, cmd)
	}
	if ok {
		_ = h.emitControl(ctx, grant.ControlledID, cmd)
	}
}

func (h *deviceHub) emitControl(ctx context.Context, uid int64, cmd deviceCommand) error {
	h.mu.Lock()
	h.sendControl(uid, cmd)
	h.mu.Unlock()
	return h.broadcast(ctx, uid, "control", cmd)
}
