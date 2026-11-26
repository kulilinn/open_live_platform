package httpapi

import (
	"net/http"
	"sort"
	"strconv"
	"time"

	"open_live_platform/internal/date"
	"open_live_platform/internal/gift"
	"open_live_platform/internal/transport/httpx"
)

func (a giftInteractionsAPI) quickList(w http.ResponseWriter, r *http.Request, actor int64) {
	q := r.URL.Query()
	who, err := strconv.ParseInt(q.Get("user_id"), 10, 64)
	if err != nil || who <= 0 {
		httpx.Error(w, http.StatusBadRequest, "invalid user ID")
		return
	}
	if who != actor {
		dateGiftResult(w, nil, date.ErrForbidden)
		return
	}
	ctx := r.Context()
	items, err := a.catalog.Items(ctx)
	if err != nil {
		dateGiftResult(w, nil, err)
		return
	}
	var ratio float64
	if value := q.Get("date_room_id"); value != "" {
		room, err := strconv.ParseInt(value, 10, 64)
		if err != nil || room <= 0 {
			httpx.Error(w, http.StatusBadRequest, "invalid room ID")
			return
		}
		if store, ok := a.dates.Service.Store.(gift.RoomEffectsStore); ok {
			ratio, err = store.GiftDurationReduction(ctx, room)
			if err != nil {
				dateGiftResult(w, nil, err)
				return
			}
		}
	}
	list := make([]gift.PromotionView, 0, len(a.content.QuickGifts))
	now := time.Now()
	for _, entry := range a.content.QuickGifts {
		item, ok := a.content.PromotionAtLimit(entry.LimitID, actor, items, now)
		if !ok {
			continue
		}
		item.QuickGiftID = entry.ID
		if item.Duration > 0 && ratio > 0 {
			item.Duration = max(1, int64(float64(item.Duration)*(100-ratio)/100))
		}
		list = append(list, item)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].QuickGiftID < list[j].QuickGiftID })
	dateGiftResult(w, list, nil)
}
