package httpapi

import (
	"net/http"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/gift"
	"open_live_platform/internal/payment"
	"open_live_platform/internal/transport/httpx"
)

func RegisterPoints(mux *http.ServeMux, accounts auth.AccountStore, billing *payment.Client) {
	mux.HandleFunc("GET /api/payment/points/get", (server{accounts: accounts}).authenticated(func(w http.ResponseWriter, r *http.Request, userID int64) {
		points, err := billing.Balance(r.Context(), userID)
		if err != nil {
			httpx.JSON(w, http.StatusServiceUnavailable, map[string]string{"status": "503", "message": "payment provider unavailable"})
			return
		}
		httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": map[string]int64{"points": points}})
	}))
}

func RegisterGiftCatalog(mux *http.ServeMux, accounts auth.AccountStore, items []gift.Gift) {
	mux.HandleFunc("GET /api/gift/list", (server{accounts: accounts}).authenticated(func(w http.ResponseWriter, r *http.Request, _ int64) {
		data := items
		switch r.URL.Query().Get("all") {
		case "", "true":
		case "false":
			data = []gift.Gift{}
		default:
			httpx.Error(w, http.StatusBadRequest, "all must be true or false")
			return
		}
		httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": data})
	}))
}
