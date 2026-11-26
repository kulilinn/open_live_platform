package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/dm"
	"open_live_platform/internal/feed"
	"open_live_platform/internal/live"
	"open_live_platform/internal/subscription"
	"open_live_platform/internal/transport/httpx"
)

func RegisterFeed(mux *http.ServeMux, accounts auth.AccountStore, rooms live.RoomStore, subscriptions subscription.Store, inbox feed.Store, messages dm.Store, moderator dm.Moderator) {
	s := subscriptionAPI{server: server{accounts: accounts, rooms: rooms}, store: subscriptions, inbox: inbox}
	const base = "/api/feed/message/"
	route := func(method, path string, f userHandler) { mux.HandleFunc(method+" "+base+path, s.authenticated(f)) }
	reply := func(w http.ResponseWriter, data any, err error) {
		if err != nil {
			httpx.Error(w, 500, "could not access inbox")
			return
		}
		res := map[string]any{"status": "200"}
		if data != nil {
			res["data"] = data
		}
		httpx.JSON(w, 200, res)
	}
	enabled := func(ctx context.Context, uid, peer int64) (bool, error) {
		_, ok, err := messages.FindDMConversation(ctx, uid, peer)
		if errors.Is(err, dm.ErrNotFound) {
			return false, nil
		}
		return ok, err
	}
	route("GET", "config", func(w http.ResponseWriter, r *http.Request, uid int64) {
		data, err := inbox.FeedConfig(r.Context(), uid)
		reply(w, data, err)
	})
	route("POST", "config", func(w http.ResponseWriter, r *http.Request, uid int64) {
		var body struct{ Dating, Dated *bool }
		if !httpx.Decode(w, r, &body) {
			return
		}
		if body.Dating == nil || body.Dated == nil {
			httpx.Error(w, 400, "dating and dated required")
			return
		}
		cfg := feed.Config{Dating: *body.Dating, Dated: *body.Dated}
		reply(w, cfg, inbox.SetFeedConfig(r.Context(), uid, cfg))
	})
	route("GET", "inbox_unreads", func(w http.ResponseWriter, r *http.Request, uid int64) {
		data, err := inbox.FeedUnread(r.Context(), uid)
		reply(w, data, err)
	})
	route("GET", "inbox", func(w http.ResponseWriter, r *http.Request, uid int64) {
		ctx := r.Context()
		entries, err := inbox.FeedEntries(ctx, uid)
		if err != nil {
			reply(w, nil, err)
			return
		}
		items := make([]map[string]any, 0, len(entries))
		for _, entry := range entries {
			u, ok, err := accounts.Find(ctx, entry.Peer)
			if err != nil {
				reply(w, nil, err)
				return
			}
			if !ok {
				continue
			}
			allowed, err := enabled(ctx, uid, entry.Peer)
			if err != nil {
				reply(w, nil, err)
				return
			}
			p := u.Profile
			if p == nil {
				p = &auth.Profile{SearchName: u.Account}
			}
			items = append(items, map[string]any{"uid": strconv.FormatInt(u.ID, 10), "display_name": u.Account, "search_name": p.SearchName,
				"photo": p.Photo, "toy_info": p.ToyInfo, "online_status": 0, "can_message": allowed, "data": entry.Data,
				"unreads": entry.Unreads, "is_anchor": p.IsAnchor, "argora_uuid": "", "room": p.GroupRoom, "teacher_label": ""})
		}
		reply(w, items, nil)
	})
	route("POST", "inbox", func(w http.ResponseWriter, r *http.Request, uid int64) {
		var body struct {
			UID  int64  `json:"uid,string"`
			Data string `json:"data"`
		}
		if !httpx.Decode(w, r, &body) {
			return
		}
		if body.UID < 1 || body.UID == uid {
			httpx.Error(w, 400, "invalid recipient")
			return
		}
		ctx := r.Context()
		if err := auth.CheckCommunication(ctx, accounts, uid); err != nil {
			dmReply(w, nil, err)
			return
		}
		_, ok, err := accounts.Find(ctx, body.UID)
		if err != nil {
			reply(w, nil, err)
			return
		}
		if !ok {
			dmReply(w, nil, dm.ErrNotFound)
			return
		}
		if moderator == nil {
			dmModerationUnavailable(w)
			return
		}
		passed, err := moderator.ApproveDirectMessage(ctx, dm.SendInput{SenderID: uid, ReceiverID: body.UID, Content: body.Data})
		if err != nil {
			dmModerationUnavailable(w)
			return
		}
		if !passed {
			dmErrorResponse(w, 200, "4061", "private chat audit reject")
			return
		}
		reply(w, body, inbox.SetFeedEntry(ctx, uid, body.UID, body.Data))
	})
	route("POST", "enabled/{fid}", func(w http.ResponseWriter, r *http.Request, uid int64) {
		if fid, ok := httpx.PathID(w, r, "fid"); ok {
			v, err := enabled(r.Context(), uid, fid)
			reply(w, v, err)
		}
	})
	mark := func(hide bool) userHandler {
		return func(w http.ResponseWriter, r *http.Request, uid int64) {
			if fid, ok := httpx.PathID(w, r, "fid"); ok {
				reply(w, nil, inbox.MarkFeedEntry(r.Context(), uid, fid, hide))
			}
		}
	}
	route("POST", "inbox/read_all/{fid}", mark(false))
	route("DELETE", "inbox/{fid}", mark(true))
	route("POST", "search", s.searchFeed(messages))
	route("POST", "welcome", func(w http.ResponseWriter, r *http.Request, uid int64) {
		var body struct {
			Data *string `json:"data"`
		}
		if !httpx.Decode(w, r, &body) {
			return
		}
		if body.Data == nil {
			httpx.Error(w, 400, "data required")
			return
		}
		ctx := r.Context()
		if _, err := s.anchor(ctx, uid); err != nil {
			if errors.Is(err, subscription.ErrForbidden) {
				err = subscription.ErrNotFound
			}
			subscriptionError(w, err)
			return
		}
		config, err := inbox.FeedConfig(ctx, uid)
		if err != nil {
			reply(w, nil, err)
			return
		}
		if *body.Data != "" && *body.Data != config.Welcome {
			if moderator == nil {
				dmModerationUnavailable(w)
				return
			}
			pass, err := moderator.ApproveDirectMessage(ctx, dm.SendInput{SenderID: uid, ReceiverID: uid, Content: *body.Data})
			if err != nil {
				dmModerationUnavailable(w)
				return
			}
			if !pass {
				dmErrorResponse(w, 200, "4062", "welcome audit reject")
				return
			}
			err = inbox.SetFeedWelcome(ctx, uid, *body.Data)
			if err != nil {
				reply(w, nil, err)
				return
			}
		}
		reply(w, true, nil)
	})
}
