package httpapi

import (
	"crypto/subtle"
	"math"
	"net/http"
	"strconv"
	"time"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/gift"
	"open_live_platform/internal/live"
	"open_live_platform/internal/subscription"
	"open_live_platform/internal/transport/httpx"
)

type earnings struct {
	store         gift.EarningsStore
	rooms         live.RoomStore
	subscriptions subscription.Store
	token         string
}

func RegisterEarnings(mux *http.ServeMux, accounts auth.AccountStore, rooms live.RoomStore, store gift.EarningsStore, subscriptions subscription.Store, token string) {
	api := earnings{store: store, rooms: rooms, subscriptions: subscriptions, token: token}
	s := server{accounts: accounts}
	mux.HandleFunc("GET /anchor/api/stat_income_sometime", s.authenticated(api.summary))
	mux.HandleFunc("GET /anchor/api/get_income_details", s.authenticated(api.details))
	mux.HandleFunc("GET /anchor/api/stat_fans_income", s.authenticated(api.dashboard))
	mux.HandleFunc("GET /api/earning/living_anchor_list", api.activeAnchors)
}

func (e earnings) activeAnchors(w http.ResponseWriter, r *http.Request) {
	inner := r.Header.Get("X-Inner-Service")
	if (inner != "admin" && inner != "mcn" && inner != "innow") || e.token == "" ||
		subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+e.token)) != 1 {
		httpx.JSON(w, http.StatusForbidden, map[string]string{"status": "403", "message": "forbidden"})
		return
	}
	rooms, err := e.rooms.Rooms(r.Context())
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "could not read active streams")
		return
	}
	ids := make([]int64, 0)
	for _, room := range rooms {
		switch room.Status() {
		case live.RoomOnline, live.RoomLiveShow, live.RoomAnchorAway:
			ids = append(ids, room.OwnerID)
		}
	}
	collectibleData(w, ids)
}

func ownEarnings(w http.ResponseWriter, r *http.Request, actor int64) bool {
	if id := r.URL.Query().Get("anchor_id"); id != "" {
		anchor, err := strconv.ParseInt(id, 10, 64)
		if err != nil || anchor < 1 {
			httpx.Error(w, http.StatusBadRequest, "invalid anchor ID")
			return false
		}
		if anchor != actor {
			httpx.JSON(w, http.StatusForbidden, map[string]string{"status": "403", "message": "forbidden"})
			return false
		}
	}
	return true
}

func earningsFilter(w http.ResponseWriter, r *http.Request, actor int64) (gift.EarningsFilter, bool) {
	if !ownEarnings(w, r, actor) {
		return gift.EarningsFilter{}, false
	}
	q := r.URL.Query()
	dateType, err := strconv.ParseInt(q.Get("date_type"), 10, 8)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid date_type")
		return gift.EarningsFilter{}, false
	}
	var incomeType *int8
	if value := q.Get("income_type"); value != "" {
		parsed, err := strconv.ParseInt(value, 10, 8)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, "invalid income_type")
			return gift.EarningsFilter{}, false
		}
		kind := int8(parsed)
		incomeType = &kind
	}
	return gift.NewEarningsFilter(actor, dateType, incomeType, time.Now()), true
}

func (e earnings) summary(w http.ResponseWriter, r *http.Request, actor int64) {
	f, ok := earningsFilter(w, r, actor)
	if !ok {
		return
	}
	buckets, err := e.store.EarningsSummary(r.Context(), f)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "could not read earnings")
		return
	}
	sum, ok := sumEarnings(w, buckets)
	if !ok {
		return
	}
	collectibleData(w, map[string]any{"list": buckets, "sum": sum})
}

func (e earnings) dashboard(w http.ResponseWriter, r *http.Request, actor int64) {
	if !ownEarnings(w, r, actor) {
		return
	}
	ctx := r.Context()
	now := time.Now().UTC()
	today := now.Truncate(24 * time.Hour)
	relationships, err := e.subscriptions.Relationships(ctx, 0, actor)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "could not read subscriptions")
		return
	}
	fans, newFans := map[int64]struct{}{}, map[int64]struct{}{}
	paidFans, newPaidFans := map[int64]struct{}{}, map[int64]struct{}{}
	for _, relationship := range relationships {
		if !relationship.ExpiresAt.After(now) {
			continue
		}
		id := relationship.FanID
		fans[id] = struct{}{}
		if relationship.CreatedAt.After(today) {
			newFans[id] = struct{}{}
		}
		if relationship.Level != 0 {
			paidFans[id] = struct{}{}
			if relationship.CreatedAt.After(today) {
				newPaidFans[id] = struct{}{}
			}
		}
	}
	buckets, err := e.store.EarningsSummary(ctx, gift.EarningsFilter{AnchorID: actor, To: now})
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "could not read earnings")
		return
	}
	total, ok := sumEarnings(w, buckets)
	if !ok {
		return
	}
	var todayTotal int64
	if len(buckets) > 0 && buckets[0].Date == today.Format("2006-01-02") {
		todayTotal = buckets[0].Points
	}
	collectibleData(w, map[string]any{"fans_total": len(fans), "fans_today_total": len(newFans),
		"pay_fans_total": len(paidFans), "pay_fans_today_total": len(newPaidFans),
		"income_total": total, "income_today_total": todayTotal})
}

func sumEarnings(w http.ResponseWriter, buckets []gift.EarningsBucket) (int64, bool) {
	var sum int64
	for _, bucket := range buckets {
		amount := bucket.Points
		if (amount > 0 && sum > math.MaxInt64-amount) || (amount < 0 && sum < math.MinInt64-amount) {
			httpx.Error(w, http.StatusInternalServerError, "earnings total exceeds supported range")
			return 0, false
		}
		sum += amount
	}
	return sum, true
}

func (e earnings) details(w http.ResponseWriter, r *http.Request, actor int64) {
	f, ok := earningsFilter(w, r, actor)
	if !ok {
		return
	}
	q := r.URL.Query()
	page, pageErr := strconv.ParseInt(q.Get("page"), 10, 32)
	size, sizeErr := strconv.ParseInt(q.Get("page_size"), 10, 32)
	if pageErr != nil || sizeErr != nil || page < 1 || size < 1 {
		httpx.Error(w, http.StatusBadRequest, "invalid earnings page")
		return
	}
	size = min(size, 100)
	lines, current, count, err := e.store.EarningsDetails(r.Context(), f, page, size)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "could not read earnings")
		return
	}
	collectibleData(w, map[string]any{"list": lines, "current": current, "pageSize": size, "total": count})
}
