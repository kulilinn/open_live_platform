package httpapi

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"open_live_platform/internal/dm"
	"open_live_platform/internal/subscription"
	"open_live_platform/internal/transport/httpx"
)

func (s subscriptionAPI) getSubscriptions(w http.ResponseWriter, r *http.Request, fanID int64) {
	var input struct {
		AnchorID subscriptionID `json:"anchor_id"`
	}
	if !decodeEmailRequest(w, r, &input) {
		return
	}
	if input.AnchorID < 1 {
		subscriptionError(w, subscription.ErrInvalidPlan)
		return
	}
	s.writeRelationships(w, r, fanID, int64(input.AnchorID))
}

func (s subscriptionAPI) getAnchorSubscriptions(w http.ResponseWriter, r *http.Request, anchorID int64) {
	if !s.requireAnchor(w, r, anchorID) {
		return
	}
	var input struct {
		AudienceID subscriptionID `json:"andience"`
	}
	if !decodeEmailRequest(w, r, &input) {
		return
	}
	if input.AudienceID < 1 {
		subscriptionError(w, subscription.ErrInvalidPlan)
		return
	}
	s.writeRelationships(w, r, int64(input.AudienceID), anchorID)
}

func (s subscriptionAPI) writeRelationships(w http.ResponseWriter, r *http.Request, fanID, anchorID int64) {
	rows, err := s.store.Relationships(r.Context(), fanID, anchorID)
	if err != nil {
		subscriptionError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": rows})
}

func (s subscriptionAPI) subscriptionList(w http.ResponseWriter, r *http.Request, fanID int64) {
	ctx := r.Context()
	rows, err := s.store.Relationships(ctx, fanID, 0)
	if err != nil {
		subscriptionError(w, err)
		return
	}
	result := []map[string]any{}
	seen := make(map[int64]bool)
	now := time.Now()
	for _, r := range rows {
		if seen[r.AnchorID] || !r.Active(now) {
			continue
		}
		user, found, err := s.accounts.Find(ctx, r.AnchorID)
		if err != nil {
			subscriptionError(w, err)
			return
		}
		if found {
			other := subscriptionDMUser(user, true)
			result = append(result, map[string]any{"uid": strconv.FormatInt(user.ID, 10), "img": other.Photo, "state": 0})
			seen[r.AnchorID] = true
		}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": map[string]any{"list": result, "new_content": 0}})
}

func (s subscriptionAPI) subscriptionPage(asAnchor bool) userHandler {
	return func(w http.ResponseWriter, r *http.Request, actor int64) {
		ctx := r.Context()
		fanID, anchorID := actor, int64(0)
		if asAnchor {
			if !s.requireAnchor(w, r, actor) {
				return
			}
			fanID, anchorID = 0, actor
		}
		query := r.URL.Query()
		page, _ := strconv.ParseInt(query.Get("page"), 10, 64)
		pageSize, _ := strconv.ParseInt(query.Get("page_size"), 10, 64)
		if pageSize == 0 {
			pageSize = 20
		}
		requested := dm.NewPageRequest(page, pageSize)
		cancelled, _ := strconv.ParseBool(query.Get("is_cancel"))
		levels := query.Get("level")
		rows, err := s.store.Relationships(ctx, fanID, anchorID)
		if err != nil {
			subscriptionError(w, err)
			return
		}
		filtered := rows[:0]
		for _, row := range rows {
			if (cancelled && row.Reason == 0) || (!cancelled && row.Expired != 0) || !wantedSubscriptionLevel(levels, row.Level) {
				continue
			}
			filtered = append(filtered, row)
		}
		_, _, pagination := requested.Window(int64(len(filtered)))
		requested.Page = min(requested.Page, pagination.TotalPages)
		offset, limit, pagination := requested.Window(int64(len(filtered)))
		result := make([]map[string]any, 0, limit)
		plansByAnchor := map[int64][]subscription.Plan{}
		for _, row := range filtered[offset : offset+limit] {
			otherID := row.AnchorID
			if asAnchor {
				otherID = row.FanID
			}
			user, found, err := s.accounts.Find(ctx, otherID)
			if err != nil {
				subscriptionError(w, err)
				return
			}
			if !found {
				continue
			}
			plans, known := plansByAnchor[row.AnchorID]
			if !known {
				plans, err = s.store.Plans(ctx, row.AnchorID, false)
				if err != nil {
					subscriptionError(w, err)
					return
				}
				plansByAnchor[row.AnchorID] = plans
			}
			var price int64
			for _, plan := range plans {
				if plan.ID == row.PlanID {
					price = plan.Price
					break
				}
			}
			other := subscriptionDMUser(user, !asAnchor)
			entry := map[string]any{
				"id": strconv.FormatInt(otherID, 10), "avatar": other.Photo, "subscription_level": row.Level,
				"create_at": row.CreatedAt, "expire_at": row.ExpiresAt, "cancel_at": row.CancelledAt, "is_expire": row.Expired != 0,
			}
			if asAnchor {
				entry["name"], entry["price"] = other.DisplayName, price
			} else {
				room, roomErr := s.rooms.Info(ctx, other.ID)
				entry["display_name"], entry["search_name"], entry["toy_info"] = other.DisplayName, other.SearchName, other.ToyInfo
				entry["plan_name"], entry["plan_id"], entry["plan_price"], entry["all_plan"] = row.PlanName, strconv.FormatInt(row.PlanID, 10), price, plans
				entry["room"], entry["online"], entry["banner"], entry["teacherLabel"] = strconv.FormatInt(other.ID, 10), "Offline", room.Banner, ""
				if roomErr == nil && room.Online {
					entry["online"] = "Living"
				}
			}
			result = append(result, entry)
		}
		httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": result, "page": pagination.Page, "pages": pagination.TotalPages})
	}
}

func wantedSubscriptionLevel(query string, level int) bool {
	if query == "" {
		return true
	}
	for part := range strings.SplitSeq(query, ",") {
		if part == strconv.Itoa(level) {
			return true
		}
	}
	return false
}
