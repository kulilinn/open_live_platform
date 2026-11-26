package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"open_live_platform/internal/live"
	"open_live_platform/internal/subscription"
	"open_live_platform/internal/transport/httpx"
)

func (s subscriptionAPI) publicFanPage(w http.ResponseWriter, r *http.Request, actor int64) {
	anchor, err := strconv.ParseInt(r.URL.Query().Get("anchor_id"), 10, 64)
	if err != nil || anchor < 1 {
		subscriptionError(w, subscription.ErrInvalidPlan)
		return
	}
	cancelled, _ := strconv.ParseBool(r.URL.Query().Get("is_cancel"))
	if cancelled && anchor != actor {
		subscriptionError(w, subscription.ErrForbidden)
		return
	}
	s.writeSubscriptionPage(w, r, 0, anchor, true)
}

func (s subscriptionAPI) subscriptionStatistics(asAnchor bool) userHandler {
	return func(w http.ResponseWriter, r *http.Request, actor int64) {
		ctx := r.Context()
		query := r.URL.Query()
		id := actor
		if raw := query.Get("query_id"); raw != "" {
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || n < 1 {
				subscriptionError(w, subscription.ErrInvalidPlan)
				return
			}
			id = n
		}
		isCancel := false
		cancelFilter := query.Get("is_cancel")
		if raw := cancelFilter; raw != "" {
			var err error
			isCancel, err = strconv.ParseBool(raw)
			if err != nil {
				subscriptionError(w, subscription.ErrInvalidPlan)
				return
			}
		}
		if isCancel && id != actor {
			subscriptionError(w, subscription.ErrForbidden)
			return
		}
		fan, anchor := id, int64(0)
		if asAnchor {
			fan, anchor = 0, id
		}
		rows, err := s.store.Relationships(ctx, fan, anchor)
		if err != nil {
			subscriptionError(w, err)
			return
		}
		var count [3]int
		now := time.Now()
		seen := map[int64]struct{}{}
		cancelled := 0
		for _, sub := range rows {
			if sub.CancelledAt != nil {
				cancelled++
			}
			if (isCancel && sub.CancelledAt == nil) || (!isCancel && !sub.Active(now)) {
				continue
			}
			other := sub.AnchorID
			if asAnchor {
				other = sub.FanID
			}
			seen[other] = struct{}{}
			if sub.Level >= 0 && sub.Level < len(count) {
				count[sub.Level]++
			}
		}
		if id != actor || (cancelFilter != "" && !isCancel) {
			cancelled = 0
		}
		writeSubscriptionData(w, map[string]int{"all": len(seen), "free": count[0], "basic": count[1],
			"premium": count[2], "pay": count[1] + count[2], "cancel": cancelled})
	}
}

func (s subscriptionAPI) fanLevel(w http.ResponseWriter, r *http.Request, actor int64) {
	anchor, ok := httpx.PathID(w, r, "anchor_id")
	if !ok {
		return
	}
	_, points, err := s.store.SubscriptionExperience(r.Context(), actor, anchor)
	if err != nil {
		subscriptionError(w, err)
		return
	}
	_, level := subscription.ExperienceLevels(0, points)
	writeSubscriptionData(w, map[string]int{"level": level})
}

func (s subscriptionAPI) roomSubscriptionLevel(w http.ResponseWriter, r *http.Request, actor int64) {
	ctx := r.Context()
	var anchor int64
	id, parseErr := strconv.ParseInt(r.PathValue("room"), 10, 64)
	if parseErr == nil && id > 0 {
		room, err := s.rooms.Info(ctx, id)
		if err != nil && !errors.Is(err, live.ErrRoomNotFound) {
			subscriptionError(w, err)
			return
		}
		anchor = room.OwnerID
	}
	platform, points, err := s.store.SubscriptionExperience(ctx, actor, anchor)
	if err != nil {
		subscriptionError(w, err)
		return
	}
	platformLevel, fanLevel := subscription.ExperienceLevels(platform, points)
	result := []map[string]any{}
	if anchor != 0 {
		rows, err := s.store.Relationships(ctx, actor, anchor)
		if err != nil {
			subscriptionError(w, err)
			return
		}
		now := time.Now()
		for _, sub := range rows {
			result = append(result, map[string]any{"plan_name": sub.PlanName, "is_expired": sub.Level == 0 || !sub.Active(now),
				"subscription_level": sub.Level})
		}
	}
	writeSubscriptionData(w, map[string]any{"platform_level": platformLevel, "fans_level": fanLevel, "subscription": result})
}
