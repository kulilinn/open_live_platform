package admin

import (
	"context"
	"crypto/sha256"
	"net/http"
	"time"

	"open_live_platform/internal/transport/httpx"
)

type PlatformStats struct {
	Users    int64 `json:"userTotal"`
	NewUsers int64 `json:"newUserTotalToday"`
	Logins   int64 `json:"loginTotalToday"`
	Online   int64 `json:"onlineTotal"`
	Streams  int64 `json:"streamingTotal"`
}

func (b backoffice) registerPlatformStats(mux *http.ServeMux) {
	store, ok := b.store.(interface {
		PlatformStats(context.Context, time.Time, time.Time) (PlatformStats, error)
	})
	if !ok {
		return
	}
	mux.HandleFunc("POST /api/operation/platform/statistic/index", b.authorized(false, true, func(w http.ResponseWriter, r *http.Request, _ Principal, _ [sha256.Size]byte) {
		var q struct{ StartTimeToday, EndTimeToday string }
		if !httpx.Decode(w, r, &q) {
			return
		}
		start, end := parsePurchaseTime(q.StartTimeToday), parsePurchaseTime(q.EndTimeToday)
		if start.IsZero() || end.Before(start) {
			backofficeError(w, 400, "400", "Invalid statistics time range")
			return
		}
		result, err := store.PlatformStats(r.Context(), start, end)
		backofficeReply(w, result, err)
	}))
}
