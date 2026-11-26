package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/transport/httpx"
)

func (s server) signUpCompat(w http.ResponseWriter, r *http.Request) {
	if !rustAuthCompat(r) {
		s.signUp(w, r)
		return
	}
	var input credentials
	if !httpx.Decode(w, r, &input) {
		return
	}
	session, err := s.accounts.SignUp(r.Context(), input.Email, input.Password, input.Account)
	if err != nil {
		sessionResponse(w, http.StatusOK, session, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"status": "200", "data": map[string]any{"user": rustAuthUser(session.User)},
	})
}

func (s server) signInCompat(w http.ResponseWriter, r *http.Request) {
	if !rustAuthCompat(r) {
		s.signIn(w, r)
		return
	}
	var input credentials
	if !httpx.Decode(w, r, &input) {
		return
	}
	session, err := s.accounts.SignIn(r.Context(), input.Email, input.Password)
	if err != nil {
		sessionResponse(w, http.StatusOK, session, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"status": "200", "token": session.Token, "user": rustAuthUser(session.User),
	})
}

func rustAuthCompat(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/vnd.livemedia+json") ||
		r.URL.Query().Get("compat") == "rust"
}

func rustAuthUser(user auth.User) map[string]any {
	return map[string]any{
		"id": strconv.FormatInt(user.ID, 10), "email": user.Email,
		"language": user.Language, "x_user_tag": "",
	}
}
