package httpapi

import (
	"context"
	"crypto/subtle"
	"net/http"
	"time"

	"open_live_platform/internal/transport/httpx"
)

type LiveIncomeCollector interface {
	CollectLiveIncome(context.Context, time.Time) error
}

func RegisterLiveIncomeSchedule(mux *http.ServeMux, token string, store LiveIncomeCollector) {
	mux.HandleFunc("GET /schedule/statistic/streamer_live_income", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if token == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			httpx.Error(w, http.StatusForbidden, "Operation not allowed")
			return
		}
		if store == nil {
			httpx.Error(w, http.StatusServiceUnavailable, "live income collection unavailable")
			return
		}
		if err := store.CollectLiveIncome(r.Context(), time.Now().UTC().Truncate(24*time.Hour)); err != nil {
			httpx.Error(w, http.StatusInternalServerError, "could not collect live income")
			return
		}
		httpx.JSON(w, http.StatusOK, map[string]any{"status": "200"})
	})
}
