package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/transport/httpx"
)

func (s server) signUpCompat(w http.ResponseWriter, r *http.Request) {
	compat := rustAuthCompat(r)
	if !compat && s.emailVerification == nil {
		s.signUp(w, r)
		return
	}
	var input struct {
		credentials
		Code string `json:"code"`
	}
	if !decodeEmailRequest(w, r, &input) {
		return
	}
	var session auth.Session
	var err error
	if s.emailVerification != nil {
		if compat && !validRustPassword(w, input.Password) {
			return
		}
		if strings.TrimSpace(input.Account) == "" || input.Password == "" {
			emailCodeError(w, auth.ErrMissingFields, compat)
			return
		}
		session, err = s.emailVerification.SignUp(r.Context(), input.Email, input.Code, input.Password, input.Account)
		if err != nil {
			emailCodeError(w, err, compat)
			return
		}
	} else {
		session, err = s.accounts.SignUp(r.Context(), input.Email, input.Password, input.Account)
	}
	if !compat {
		sessionResponse(w, http.StatusCreated, session, err)
		return
	}
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
	signIn := s.accounts.SignIn
	if accounts, ok := s.accounts.(*auth.JWTAccounts); ok {
		signIn = accounts.SignInJWT
	}
	session, err := signIn(r.Context(), input.Email, input.Password)
	if writeBanned(w, err, true) {
		return
	}
	if err != nil {
		sessionResponse(w, http.StatusOK, session, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, map[string]any{
		"status": "200", "token": session.Token, "user": rustAuthUser(session.User),
	})
}

func (s server) sessionCompat(w http.ResponseWriter, r *http.Request) {
	token, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !rustAuthCompat(r) && !auth.IsJWT(token) {
		s.authenticated(s.session)(w, r)
		return
	}
	if !found {
		httpx.Error(w, http.StatusUnauthorized, "invalid bearer token")
		return
	}
	session, ok, err := s.accounts.Refresh(r.Context(), token)
	if writeBanned(w, err, true) {
		return
	}
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "could not refresh session")
		return
	}
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "invalid bearer token")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, map[string]any{
		"status": "200", "token": session.Token,
	})
}

func rustAuthCompat(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/vnd.livemedia+json") ||
		r.URL.Query().Get("compat") == "rust"
}

func (s server) appToken(w http.ResponseWriter, r *http.Request, _ int64) {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	var session auth.Session
	var err error
	if auth.IsJWT(token) {
		var code string
		code, err = auth.NewLoginCode(r.Context(), s.accounts, token)
		if err == nil {
			session, err = auth.ExchangeLoginCode(r.Context(), s.accounts, code)
		}
	} else {
		var ok bool
		session, ok, err = s.accounts.Refresh(r.Context(), token)
		if err == nil && !ok {
			err = auth.ErrInvalidCredentials
		}
	}
	if writeBanned(w, err, true) {
		return
	}
	if err != nil {
		httpx.Error(w, 401, "could not issue application token")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, 200, map[string]string{"status": "200", "token": session.Token})
}

func rustAuthUser(user auth.User) map[string]any {
	return map[string]any{
		"id": strconv.FormatInt(user.ID, 10), "email": user.Email,
		"language": user.Language, "x_user_tag": "",
	}
}
