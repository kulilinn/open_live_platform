package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/transport/httpx"
)

func (s server) exchangeCode(w http.ResponseWriter, r *http.Request) {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	code, err := auth.NewLoginCode(r.Context(), s.accounts, token)
	if err != nil {
		exchangeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": map[string]string{"code": code}})
}

func (s server) exchangeToken(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Code string `json:"code"`
	}
	if !decodeEmailRequest(w, r, &input) {
		return
	}
	session, err := auth.ExchangeLoginCode(r.Context(), s.accounts, input.Code)
	if err != nil {
		exchangeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "token": session.Token, "user": rustAuthUser(session.User)})
}

func exchangeError(w http.ResponseWriter, err error) {
	if writeBanned(w, err, true) {
		return
	}
	switch {
	case errors.Is(err, auth.ErrExchangeUnavailable):
		httpx.Error(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, auth.ErrInvalidCredentials):
		w.Header().Set("WWW-Authenticate", "Bearer")
		httpx.Error(w, http.StatusUnauthorized, "invalid bearer token")
	case errors.Is(err, auth.ErrInvalidLoginCode):
		httpx.Error(w, http.StatusUnauthorized, err.Error())
	default:
		httpx.Error(w, http.StatusInternalServerError, "could not exchange login code")
	}
}
