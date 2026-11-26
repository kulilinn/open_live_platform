package httpapi

import (
	"errors"
	"net/http"
	"open_live_platform/internal/auth"
	"open_live_platform/internal/transport/httpx"
	"strings"
)

func RegisterTelegram(mux *http.ServeMux, accounts auth.AccountStore, store auth.JWTBackend, token string) {
	mux.HandleFunc("GET /api/auth/tg", func(w http.ResponseWriter, r *http.Request) {
		if token == "" {
			httpx.Error(w, 503, "Telegram login unavailable")
			return
		}
		raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "tma")
		if !ok {
			httpx.Error(w, 401, "invalid Telegram login")
			return
		}
		s, err := auth.SignInTelegram(r.Context(), accounts, store, token, strings.TrimSpace(raw), r.Header.Get("x-loverose-lang"))
		if writeBanned(w, err, true) {
			return
		}
		switch {
		case errors.Is(err, auth.ErrTelegramLogin):
			httpx.Error(w, 401, err.Error())
		case errors.Is(err, auth.ErrTelegramCollision):
			httpx.Error(w, 409, err.Error())
		case err != nil:
			httpx.Error(w, 500, "could not sign in")
		default:
			w.Header().Set("Cache-Control", "no-store")
			httpx.JSON(w, 200, map[string]string{"status": "200", "token": s.Token})
		}
	})
}
