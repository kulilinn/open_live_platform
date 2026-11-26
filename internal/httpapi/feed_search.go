package httpapi

import (
	"cmp"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/dm"
	"open_live_platform/internal/subscription"
	"open_live_platform/internal/transport/httpx"
)

type feedSearchResult struct {
	ID           int64     `json:"uid,string"`
	SearchName   string    `json:"search_name"`
	DisplayName  string    `json:"display_name"`
	PlanName     string    `json:"plan_name"`
	PlanPrice    string    `json:"plan_price"`
	PlanType     string    `json:"plan_type"`
	Level        int       `json:"subscription_level"`
	Avatar       string    `json:"avatar"`
	Updated      time.Time `json:"update_at"`
	Room         string    `json:"room"`
	TeacherLabel string    `json:"teacherLabel"`
	ToyInfo      string    `json:"toyInfo"`
}

func (s subscriptionAPI) searchFeed(messages dm.Store) userHandler {
	return func(w http.ResponseWriter, r *http.Request, actor int64) {
		var body struct {
			Name     string
			Page     int
			ListType int `json:"list_type"`
		}
		if !httpx.Decode(w, r, &body) {
			return
		}
		ctx := r.Context()
		_, err := s.anchor(ctx, actor)
		isAnchor := err == nil
		if err != nil && !errors.Is(err, subscription.ErrForbidden) {
			subscriptionError(w, err)
			return
		}
		if body.Page <= 0 || (isAnchor && body.ListType != 1 && body.ListType != 2) {
			httpx.Error(w, 400, "invalid page or list_type")
			return
		}
		fans := isAnchor && body.ListType == 1
		fanID, anchorID := actor, int64(0)
		if fans {
			fanID, anchorID = 0, actor
		}
		rows, err := s.store.Relationships(ctx, fanID, anchorID)
		if err != nil {
			subscriptionError(w, err)
			return
		}
		highest := make(map[int64]subscription.Relationship)
		now := time.Now()
		for _, row := range rows {
			if !row.Active(now) {
				continue
			}
			peer := row.AnchorID
			if fans {
				peer = row.FanID
			}
			if current, ok := highest[peer]; !ok || current.Level < row.Level {
				highest[peer] = row
			}
		}
		results := []feedSearchResult{}
		if len(highest) > 0 {
			latest, err := messages.LatestDMFromSenders(ctx, actor)
			if err != nil {
				subscriptionError(w, err)
				return
			}
			prefix := strings.ToLower(body.Name)
			for id, row := range highest {
				u, ok, err := s.accounts.Find(ctx, id)
				if err != nil {
					subscriptionError(w, err)
					return
				}
				if !ok || !strings.HasPrefix(strings.ToLower(u.Account), prefix) {
					continue
				}
				p := u.Profile
				if p == nil {
					p = &auth.Profile{SearchName: u.Account}
				}
				updated := latest[id]
				if updated.IsZero() {
					updated = time.Unix(0, 0).UTC()
				}
				results = append(results, feedSearchResult{ID: id, SearchName: p.SearchName, DisplayName: u.Account, PlanName: row.PlanName,
					PlanPrice: strconv.FormatInt(row.PlanPrice, 10), Level: row.Level, Avatar: p.Photo, Updated: updated, ToyInfo: cmp.Or(p.ToyInfo, "{}")})
			}
		}
		slices.SortFunc(results, func(a, b feedSearchResult) int {
			if n := b.Updated.Compare(a.Updated); n != 0 {
				return n
			}
			return cmp.Compare(b.ID, a.ID)
		})
		const size = 20
		pages := (len(results) + size - 1) / size
		page := min(body.Page, pages)
		start := max(page-1, 0) * size
		httpx.JSON(w, 200, map[string]any{"status": "200", "data": results[start:min(start+size, len(results))], "page": page, "pages": pages})
	}
}
