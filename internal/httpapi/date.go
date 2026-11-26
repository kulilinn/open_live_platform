package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"math"
	"net/http"
	"strconv"
	"time"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/date"
	"open_live_platform/internal/payment"
	"open_live_platform/internal/relay"
	"open_live_platform/internal/rtc"
	"open_live_platform/internal/stt"
	"open_live_platform/internal/transport/httpx"
)

type Dates struct {
	Accounts  auth.AccountStore
	Service   *date.Service
	Signer    rtc.Signer
	Subtitles *stt.Service
	Push      relay.PushPrivate
}

func (a *Dates) Register(mux *http.ServeMux) {
	s := server{accounts: a.Accounts}
	for pattern, handler := range map[string]userHandler{
		"POST /api/live/base-setting": a.baseSetting, "POST /api/live/session-setting": a.sessionSetting,
		"GET /api/live/state/{uid}": a.state, "POST /api/live/start": a.start, "POST /api/live/stop": a.stop,
		"POST /api/live/online":          a.online,
		"POST /api/live/callin/{anchor}": a.callin, "DELETE /api/live/callin/{anchor}": a.cancel,
		"GET /api/live/callins/{anchor}": a.callins, "POST /api/live/tip/{anchor}": a.tip,
		"POST /api/live/callout/{audience}": a.callout, "GET /api/live/duration/{channel}": a.duration,
		"GET /api/live/alive/{channel}": a.alive, "POST /api/live/leave/{channel}": a.leave,
		"GET /api/live/checkout/{channel}":   a.checkout,
		"POST /api/live/stt-start/{channel}": a.sttStart, "GET /api/live/stt-query/{channel}": a.sttQuery,
		"POST /api/live/stt-update/{channel}": a.sttUpdate, "POST /api/live/stt-stop/{channel}": a.sttStop,
	} {
		mux.HandleFunc(pattern, s.authenticated(handler))
	}
}

func (a *Dates) baseSetting(w http.ResponseWriter, r *http.Request, uid int64) {
	if !a.isAnchor(w, r, uid) {
		return
	}
	var jsonInput json.RawMessage
	if !httpx.Decode(w, r, &jsonInput) {
		return
	}
	var input struct {
		Costs *struct {
			Minutes string `json:"time"`
			PerMin  int64  `json:"cost"`
		} `json:"datingCosts"`
	}
	if json.Unmarshal(jsonInput, &input) != nil || input.Costs == nil {
		dateResponse(w, date.ErrPricing, nil)
		return
	}
	minutes, err := strconv.ParseInt(input.Costs.Minutes, 10, 64)
	if err != nil || minutes < 0 || input.Costs.PerMin < 0 || (minutes > 0 && input.Costs.PerMin > math.MaxInt64/minutes) {
		dateResponse(w, date.ErrPricing, nil)
		return
	}
	err = a.Service.Store.ChangeDateHost(r.Context(), uid, func(host *date.Host, _ *date.Session, _ time.Time) error {
		host.BaseSettings, host.InitialMins, host.PricePerMin = jsonInput, minutes, input.Costs.PerMin
		return nil
	})
	dateResponse(w, err, nil)
}

func (a *Dates) sessionSetting(w http.ResponseWriter, r *http.Request, uid int64) {
	if !a.isAnchor(w, r, uid) {
		return
	}
	var settings map[string]json.RawMessage
	if !httpx.Decode(w, r, &settings) {
		return
	}
	if settings == nil {
		dateResponse(w, date.ErrPricing, nil)
		return
	}
	data, err := json.Marshal(settings)
	if err == nil {
		err = a.Service.Store.ChangeDateHost(r.Context(), uid, func(host *date.Host, _ *date.Session, _ time.Time) error {
			host.Settings = data
			return nil
		})
	}
	if err != nil {
		dateResponse(w, err, nil)
		return
	}
	a.writeState(w, r, uid, true)
}

func (a *Dates) isAnchor(w http.ResponseWriter, r *http.Request, uid int64) bool {
	user, ok, err := a.Accounts.Find(r.Context(), uid)
	if err != nil || !ok || user.Profile == nil || !user.Profile.IsAnchor || user.Moderation.Banned() {
		if err == nil {
			err = date.ErrForbidden
		}
		dateResponse(w, err, nil)
		return false
	}
	return true
}

func (a *Dates) state(w http.ResponseWriter, r *http.Request, uid int64) {
	anchor, ok := dateID(w, r, "uid", uid)
	if ok {
		a.writeState(w, r, anchor, anchor == uid)
	}
}

func (a *Dates) writeState(w http.ResponseWriter, r *http.Request, anchor int64, owner bool) {
	user, ok, err := a.Accounts.Find(r.Context(), anchor)
	if err != nil || !ok {
		if err == nil {
			err = date.ErrNotFound
		}
		dateResponse(w, err, nil)
		return
	}
	host, session, err := a.Service.Store.DateHost(r.Context(), anchor)
	if err != nil {
		dateResponse(w, err, nil)
		return
	}
	status := 0
	var channel any
	calls := []date.Invitation{}
	if host.Listening(time.Now()) || session.Active(time.Now()) || host.Pending.Active(time.Now()) {
		status = 1
		if session.Active(time.Now()) || host.Pending.Active(time.Now()) {
			status = 2
		}
		if owner {
			channel = strconv.FormatInt(host.Channel, 10)
			if host.Pending.Active(time.Now()) {
				calls = append(calls, *host.Pending)
			}
		}
	}
	name, avatar := dateProfile(user)
	result := map[string]any{
		"settings": host.Settings, "baseSetting": host.BaseSettings, "potentialEarnings": 2000, "currency": "$", "callin": "$",
		"anchorStatus": map[string]any{"uid": strconv.FormatInt(anchor, 10), "name": name, "displayName": user.Account,
			"avatar": avatar, "status": status, "channel": channel, "callIns": calls, "likes": 0},
	}
	dateResponse(w, nil, map[string]any{"result": result})
}

func (a *Dates) online(w http.ResponseWriter, r *http.Request, uid int64) {
	dateResponse(w, a.Service.RefreshOnline(r.Context(), uid), nil)
}

func (a *Dates) start(w http.ResponseWriter, r *http.Request, uid int64) {
	err := a.Service.Start(r.Context(), uid)
	dateResponse(w, err, map[string]any{"limit_ty": 0, "limit_allow": true, "limit_total": 0, "limit_duration": 0, "limit_reason": []string{}})
}

func (a *Dates) callin(w http.ResponseWriter, r *http.Request, uid int64) {
	anchor, ok := dateID(w, r, "anchor", uid)
	if !ok {
		return
	}
	user, _, err := a.Accounts.Find(r.Context(), uid)
	if err != nil {
		dateResponse(w, err, nil)
		return
	}
	name, avatar := dateProfile(user)
	invite, err := a.Service.CallIn(r.Context(), uid, anchor, date.Invitation{UID: uid, NID: date.AudienceNID,
		Name: name, DisplayName: user.Account, Avatar: avatar, Email: user.Email, Language: r.Header.Get(sttLanguageHeader)})
	if err == nil {
		a.push(r.Context(), anchor, "callin", invite)
	}
	dateResponse(w, err, nil)
}

func (a *Dates) callins(w http.ResponseWriter, r *http.Request, uid int64) {
	anchor, ok := dateID(w, r, "anchor", uid)
	if !ok {
		return
	}
	if uid != anchor {
		dateResponse(w, date.ErrForbidden, nil)
		return
	}
	host, _, err := a.Service.Store.DateHost(r.Context(), anchor)
	calls := []date.Invitation{}
	if host.Pending.Active(time.Now()) {
		calls = append(calls, *host.Pending)
	}
	dateResponse(w, err, map[string]any{"callins": calls})
}

func (a *Dates) cancel(w http.ResponseWriter, r *http.Request, uid int64) {
	anchor, ok := dateID(w, r, "anchor", uid)
	if !ok {
		return
	}
	audience, err := a.Service.CancelInvitation(r.Context(), uid, anchor)
	if err == nil && audience != 0 {
		a.inviteEnded(r.Context(), anchor, audience, uid == anchor)
	}
	dateResponse(w, err, nil)
}

func (a *Dates) tip(w http.ResponseWriter, r *http.Request, uid int64) {
	anchor, ok := dateID(w, r, "anchor", uid)
	if !ok {
		return
	}
	var input struct{ Tips int64 }
	if !httpx.Decode(w, r, &input) {
		return
	}
	invite, err := a.Service.Tip(r.Context(), uid, anchor, input.Tips)
	if err == nil {
		a.push(r.Context(), anchor, "tips", invite)
	}
	dateResponse(w, err, nil)
}

func (a *Dates) callout(w http.ResponseWriter, r *http.Request, anchor int64) {
	audience, ok := dateID(w, r, "audience", anchor)
	if !ok || !a.isAnchor(w, r, anchor) {
		return
	}
	if !a.Signer.Configured() {
		httpx.Error(w, http.StatusServiceUnavailable, "RTC signing is not configured")
		return
	}
	session, err := a.Service.CallOut(r.Context(), anchor, audience, r.Header.Get(sttLanguageHeader))
	if errors.Is(err, payment.ErrInsufficientPoints) {
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "4024", "message": err.Error()})
		return
	}
	if err != nil {
		dateResponse(w, err, nil)
		return
	}
	for _, uid := range []int64{anchor, audience} {
		if ticket, err := a.rtcTicket(r.Context(), session, uid); err == nil {
			a.push(r.Context(), uid, "callout", ticket)
		}
	}
	dateResponse(w, nil, nil)
}

type dateTicket struct {
	rtc.Token
	RemoteUID    string `json:"remoteUid"`
	RemoteNID    string `json:"remoteNid"`
	RemoteName   string `json:"remoteName"`
	RemoteAvatar string `json:"remoteAvatar"`
	ToyInfo      string `json:"toyInfo"`
}

func (a *Dates) rtcTicket(ctx context.Context, session date.Session, uid int64) (dateTicket, error) {
	nid, remote, remoteNID := session.Participant(uid)
	user, ok, err := a.Accounts.Find(ctx, remote)
	if err != nil {
		return dateTicket{}, err
	}
	if !ok || user.Moderation.Banned() {
		return dateTicket{}, date.ErrForbidden
	}
	_, avatar := dateProfile(user)
	toy := "{}"
	if user.Profile != nil && user.Profile.ToyInfo != "" {
		toy = user.Profile.ToyInfo
	}
	token, err := a.Signer.ForRoom(session.Channel, uid, nid, uid == session.AnchorID)
	if err == nil && token.Token == "" {
		err = errors.New("RTC signing is not configured")
	}
	return dateTicket{Token: token, RemoteUID: strconv.FormatInt(remote, 10), RemoteNID: strconv.FormatUint(uint64(remoteNID), 10),
		RemoteName: user.Account, RemoteAvatar: avatar, ToyInfo: toy}, err
}

func (a *Dates) roomToken(w http.ResponseWriter, r *http.Request, channel, uid int64, role string) bool {
	session, err := a.Service.SessionFor(r.Context(), channel, uid, true)
	if errors.Is(err, date.ErrNotFound) {
		return false
	}
	if err == nil && ((uid == session.AnchorID && role != "1") || (uid == session.AudienceID && role != "2")) {
		err = date.ErrForbidden
	}
	if err == nil {
		if err = auth.CheckCommunication(r.Context(), a.Accounts, uid); err == nil {
			var ticket dateTicket
			ticket, err = a.rtcTicket(r.Context(), session, uid)
			if err == nil {
				w.Header().Set("Cache-Control", "no-store")
				dateResponse(w, nil, map[string]any{"data": ticket})
				return true
			}
		}
	}
	dateResponse(w, err, nil)
	return true
}

func (a *Dates) duration(w http.ResponseWriter, r *http.Request, uid int64) {
	channel, ok := dateID(w, r, "channel", 0)
	if !ok {
		return
	}
	session, joined, err := a.Service.Heartbeat(r.Context(), channel, uid)
	if joined {
		a.pushBoth(r.Context(), session, uid, "join")
	}
	if !session.EndedAt.IsZero() {
		a.end(r.Context(), session, uid)
	}
	if errors.Is(err, payment.ErrInsufficientPoints) {
		a.push(r.Context(), uid, "point_exhausted", map[string]int64{"pt": session.Points})
		err = date.ErrClosed
	}
	dateResponse(w, err, map[string]any{"data": session.Seconds(time.Now())})
}

func (a *Dates) alive(w http.ResponseWriter, r *http.Request, uid int64) {
	channel, ok := dateID(w, r, "channel", 0)
	if ok {
		_, err := a.Service.SessionFor(r.Context(), channel, uid, true)
		dateResponse(w, err, map[string]any{"data": "alive"})
	}
}

func (a *Dates) leave(w http.ResponseWriter, r *http.Request, uid int64) {
	channel, ok := dateID(w, r, "channel", 0)
	if !ok {
		return
	}
	session, changed, err := a.Service.Leave(r.Context(), channel, uid)
	if err == nil && changed {
		a.end(r.Context(), session, uid)
	}
	dateResponse(w, err, nil)
}

func (a *Dates) stop(w http.ResponseWriter, r *http.Request, uid int64) {
	if !a.isAnchor(w, r, uid) {
		return
	}
	ended, invitee, err := a.Service.Stop(r.Context(), uid)
	if err != nil {
		dateResponse(w, err, nil)
		return
	}
	if ended.Channel != 0 {
		a.end(r.Context(), ended, uid)
	}
	if invitee != 0 {
		a.push(r.Context(), invitee, "stop", map[string]string{"uid": strconv.FormatInt(uid, 10), "channel": "0"})
	}
	a.writeState(w, r, uid, true)
}

func (a *Dates) checkout(w http.ResponseWriter, r *http.Request, uid int64) {
	channel, ok := dateID(w, r, "channel", 0)
	if !ok {
		return
	}
	session, err := a.Service.SessionFor(r.Context(), channel, uid, false)
	if err != nil {
		dateResponse(w, err, nil)
		return
	}
	anchor, _, err := a.Accounts.Find(r.Context(), session.AnchorID)
	name, photo := dateProfile(anchor)
	data := map[string]any{"channel": strconv.FormatInt(channel, 10), "start": session.StartedAt.Unix(), "time": session.Seconds(time.Now()),
		"gifts": 0, "giftTime": 0, "name": name, "displayName": anchor.Account, "avatar": photo}
	if uid == session.AnchorID {
		data["points"] = session.Points
	}
	dateResponse(w, err, map[string]any{"data": data})
}

func (a *Dates) push(ctx context.Context, uid int64, name string, payload any) {
	if a.Push != nil {
		_ = a.Push(ctx, uid, name, payload)
	}
}

func (a *Dates) pushBoth(ctx context.Context, session date.Session, uid int64, name string) {
	nid, _, _ := session.Participant(uid)
	payload := map[string]any{"uid": strconv.FormatInt(uid, 10), "nid": strconv.FormatUint(uint64(nid), 10),
		"channel": strconv.FormatInt(session.Channel, 10), "ts": time.Now().Unix()}
	for _, participant := range []int64{session.AnchorID, session.AudienceID} {
		a.push(ctx, participant, name, payload)
	}
}

func (a *Dates) end(ctx context.Context, session date.Session, uid int64) {
	a.pushBoth(ctx, session, uid, "leave")
	if a.Subtitles.Enabled() {
		_ = a.Subtitles.Stop(ctx, session.Channel, uid)
	}
}

func (a *Dates) inviteEnded(ctx context.Context, anchor, audience int64, refused bool) {
	content := "callin_close"
	if refused {
		content = "callin_refuse"
	}
	for _, uid := range []int64{anchor, audience} {
		a.push(ctx, uid, content, map[string]string{})
	}
}

func (a *Dates) Sweep(ctx context.Context) error {
	return errors.Join(a.Service.Sweep(ctx, func(session date.Session) { a.end(ctx, session, session.AnchorID) },
		func(anchor, audience int64) { a.inviteEnded(ctx, anchor, audience, false) }), a.Subtitles.Sweep(ctx))
}

func dateID(w http.ResponseWriter, r *http.Request, key string, self int64) (int64, bool) {
	if r.PathValue(key) == "@self" && self != 0 {
		return self, true
	}
	id, err := strconv.ParseInt(r.PathValue(key), 10, 64)
	if err != nil || id <= 0 {
		dateResponse(w, date.ErrNotFound, nil)
		return 0, false
	}
	return id, true
}

func dateProfile(user auth.User) (name, avatar string) {
	if user.Profile != nil {
		name, avatar = user.Profile.Name, user.Profile.Photo
	}
	return name, avatar
}

func dateResponse(w http.ResponseWriter, err error, body map[string]any) {
	if err == nil {
		if body == nil {
			body = make(map[string]any)
		}
		body["status"] = "200"
		httpx.JSON(w, http.StatusOK, body)
		return
	}
	code, status := http.StatusOK, "500"
	switch {
	case errors.Is(err, date.ErrForbidden), errors.Is(err, auth.ErrCommunicationRestricted):
		code, status = http.StatusForbidden, "403"
	case errors.Is(err, date.ErrNotFound):
		status = "4005"
	case errors.Is(err, date.ErrClosed):
		status = "4022"
	case errors.Is(err, date.ErrSelf):
		status = "4020"
	case errors.Is(err, date.ErrBusy), errors.Is(err, date.ErrNotListening):
		status = "4021"
	case errors.Is(err, date.ErrNoInvitation):
		status = "4026"
	case errors.Is(err, date.ErrCooldown):
		status = "4018"
	case errors.Is(err, date.ErrAdvanced):
		status = "4023"
	case errors.Is(err, payment.ErrInsufficientPoints):
		status = "4002"
	case errors.Is(err, date.ErrPricing):
		code, status = http.StatusBadRequest, "400"
	case errors.Is(err, payment.ErrUnavailable):
		code = http.StatusServiceUnavailable
	default:
		code = http.StatusInternalServerError
	}
	message := err.Error()
	if code >= http.StatusInternalServerError {
		log.Printf("1v1 request failed: %v", err)
		message = http.StatusText(code)
	}
	httpx.JSON(w, code, map[string]string{"status": status, "message": message})
}
