package httpapi

import (
	"net/http"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/gift"
	"open_live_platform/internal/transport/httpx"
)

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
