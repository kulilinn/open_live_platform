package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/catalog"
	"open_live_platform/internal/feed"
	"open_live_platform/internal/live"
	"open_live_platform/internal/subscription"
	"open_live_platform/internal/transport/httpx"
)

func RegisterPWA(mux *http.ServeMux, accounts auth.AccountStore, rooms live.RoomStore, subs subscription.Store, inbox feed.Store, reference catalog.Store) {
	s := subscriptionAPI{server: server{accounts: accounts, rooms: rooms}, store: subs, inbox: inbox}
	mux.HandleFunc("GET /api/get_anchor", s.authenticated(func(w http.ResponseWriter, r *http.Request, _ int64) {
		name := r.URL.Query().Get("search_name")
		if name == "" {
			httpx.Error(w, 400, "search_name required")
			return
		}
		id, found, err := accounts.FindBySearchName(r.Context(), name)
		if err == nil && !found {
			err = subscription.ErrNotFound
		}
		if err == nil {
			_, err = s.anchor(r.Context(), id)
		}
		if errors.Is(err, subscription.ErrForbidden) {
			err = subscription.ErrNotFound
		}
		if err != nil {
			subscriptionError(w, err)
			return
		}
		httpx.JSON(w, 200, map[string]any{"status": "200", "data": strconv.FormatInt(id, 10)})
	}))
	mux.HandleFunc("GET /api/pwa/anchor", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		var relationships []subscription.Relationship
		if header := r.Header.Get("Authorization"); header != "" {
			user, ok, err := accounts.Bearer(ctx, header)
			if err != nil {
				subscriptionError(w, err)
				return
			}
			if !ok || user.Moderation.Banned() {
				httpx.Error(w, 401, "invalid bearer token")
				return
			}
			relationships, err = subs.Relationships(ctx, user.ID, 0)
			if err != nil {
				subscriptionError(w, err)
				return
			}
		}
		active := map[int64]bool{}
		now := time.Now()
		for _, v := range relationships {
			if v.Active(now) {
				active[v.AnchorID] = true
			}
		}
		rows, err := reference.CatalogRows(ctx, "pwa")
		if err != nil {
			subscriptionError(w, err)
			return
		}
		data := []map[string]any{}
		for _, v := range rows {
			id := v.Int("id")
			u, found, err := accounts.Find(ctx, id)
			if err != nil {
				subscriptionError(w, err)
				return
			}
			if !found {
				continue
			}
			search := u.Account
			if u.Profile != nil {
				search = u.Profile.SearchName
			}
			data = append(data, map[string]any{"id": strconv.FormatInt(id, 10), "pwa_id": v["pwa_id"], "game_url": v["game_url"], "icon": v["icon"],
				"display_name": u.Account, "search_name": search, "sub_status": active[id], "works": v["works"], "image": v["image"], "weight": v["weight"]})
		}
		httpx.JSON(w, 200, map[string]any{"status": "200", "data": data})
	})
	mux.HandleFunc("POST /api/pwa/sub", s.authenticated(func(w http.ResponseWriter, r *http.Request, uid int64) {
		var input struct{ ID []string }
		if !httpx.Decode(w, r, &input) {
			return
		}
		if input.ID == nil {
			httpx.Error(w, 400, "id required")
			return
		}
		ctx := r.Context()
		for _, raw := range input.ID[:min(100, len(input.ID))] {
			id, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				subscriptionError(w, subscription.ErrNotFound)
				return
			}
			list, err := subs.Plans(ctx, id, true)
			if err != nil {
				subscriptionError(w, err)
				return
			}
			var plan subscription.Plan
			for _, v := range list {
				if v.Type == subscription.Free {
					plan = v
					break
				}
			}
			if plan.ID == 0 {
				subscriptionError(w, subscription.ErrNotFound)
				return
			}
			switch {
			case plan.Published == 0:
				httpx.JSON(w, 200, map[string]string{"status": "4075", "message": "free plan unpublished"})
				return
			case plan.Deleted != 0:
				httpx.JSON(w, 200, map[string]string{"status": "4076", "message": "free plan deleted"})
				return
			}
			m, err := s.members(ctx, uid, id)
			if err == nil {
				err = subs.SubscribeFree(ctx, m)
			}
			if err != nil {
				subscriptionError(w, err)
				return
			}
		}
		httpx.JSON(w, 200, map[string]any{"status": "200", "data": struct{}{}})
	}))
}
