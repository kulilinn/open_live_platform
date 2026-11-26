package httpapi

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/transport/httpx"
)

type trustedAuthAPI struct {
	directory auth.UserDirectory
	accounts  auth.AccountStore
	backend   auth.JWTBackend
	google    auth.GoogleLogin
	clients   auth.TrustedClients
	grants    auth.TrustedGrantStore
}

func RegisterTrustedAuth(mux *http.ServeMux, directory auth.UserDirectory, backend auth.JWTBackend,
	google auth.GoogleLogin, clients auth.TrustedClients) {
	grants, _ := backend.(auth.TrustedGrantStore)
	s := trustedAuthAPI{directory: directory, accounts: directory.Accounts, backend: backend, google: google, clients: clients, grants: grants}
	mux.HandleFunc("GET /api/auth_internal/get_auth_code", s.authCode)
	mux.HandleFunc("POST /api/auth_internal/check_auth_code", s.checkAuthCode)
	for path, handler := range map[string]http.HandlerFunc{
		"GET /api/microservices/user/{id}":             s.getUser,
		"POST /api/microservices/user/verify_password": s.verifyPassword,
		"POST /api/microservices/user/change_password": s.changePassword,
		"POST /api/microservices/user/verify_google":   s.verifyGoogle,
		"POST /api/microservices/user/verify_code":     s.verifyCode,
		"POST /api/microservices/user/exchange_code":   s.exchangeCode,
	} {
		mux.HandleFunc(path, s.serviceOnly(handler))
	}
}

func (s trustedAuthAPI) authCode(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if len(s.clients) == 0 {
		trustedAuthError(w, auth.ErrExchangeUnavailable)
		return
	}
	query := r.URL.Query()
	userID, err := strconv.ParseInt(query.Get("user_id"), 10, 64)
	requested := query.Get("redirect_uri")
	state := query.Get("state")
	if err != nil || userID < 1 || requested == "" || query.Get("response_type") != "code" || len(state) > 2048 {
		httpx.Error(w, http.StatusBadRequest, "invalid authorization request")
		return
	}
	client := query.Get("client_id")
	redirect, ok := s.clients.Redirect(client, requested)
	if !ok {
		httpx.Error(w, http.StatusBadRequest, "invalid OAuth client or redirect")
		return
	}
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	binding := auth.GrantBinding{Purpose: auth.OAuthCode, ClientID: client, Redirect: redirect, UserID: userID}
	code, err := auth.IssueTrustedGrant(r.Context(), s.accounts, s.grants, token, binding)
	if err != nil {
		trustedAuthError(w, err)
		return
	}
	u, err := url.Parse(redirect)
	if err != nil {
		trustedAuthError(w, err)
		return
	}
	params := u.Query()
	params.Set("code", code)
	params.Set("user_id", strconv.FormatInt(userID, 10))
	params.Set("state", state)
	u.RawQuery = params.Encode()
	httpx.JSON(w, http.StatusOK, u.String())
}

func (s trustedAuthAPI) checkAuthCode(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if len(s.clients) == 0 {
		trustedAuthError(w, auth.ErrExchangeUnavailable)
		return
	}
	var input struct {
		Client   string         `json:"client_id"`
		Redirect string         `json:"redirect_uri"`
		ID       subscriptionID `json:"user_id"`
		Code     string         `json:"auth_code"`
	}
	if !decodeEmailRequest(w, r, &input) {
		return
	}
	if !s.clients.Authorized(input.Client, r.Header.Get("Authorization")) {
		httpx.Error(w, http.StatusForbidden, "untrusted OAuth client")
		return
	}
	redirect, ok := s.clients.Redirect(input.Client, input.Redirect)
	if !ok || input.ID < 1 {
		trustedAuthError(w, auth.ErrTrustedGrant)
		return
	}
	binding := auth.GrantBinding{Purpose: auth.OAuthCode, ClientID: input.Client, Redirect: redirect, UserID: int64(input.ID)}
	user, err := auth.VerifyTrustedGrant(r.Context(), s.accounts, s.grants, input.Code, binding)
	if err != nil {
		trustedAuthError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, auth.DirectoryView(user))
}

func (s trustedAuthAPI) serviceOnly(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !s.directory.Authorized(r.Header.Get("X-Inner-Service"), r.Header.Get("Authorization")) {
			httpx.Error(w, http.StatusForbidden, "untrusted user service")
			return
		}
		handler(w, r)
	}
}

func (s trustedAuthAPI) getUser(w http.ResponseWriter, r *http.Request) {
	id, ok := httpx.PathID(w, r, "id")
	if !ok {
		return
	}
	view, err := s.directory.Get(r.Context(), id)
	if err != nil {
		trustedAuthError(w, err)
		return
	}
	writeSubscriptionData(w, view)
}

func (s trustedAuthAPI) verifyPassword(w http.ResponseWriter, r *http.Request) {
	var input struct{ Email, Password string }
	if !decodeEmailRequest(w, r, &input) {
		return
	}
	user, err := s.backend.VerifyPassword(r.Context(), input.Email, input.Password)
	if err != nil {
		trustedAuthError(w, err)
		return
	}
	if user.Moderation.Banned() {
		trustedAuthError(w, auth.BannedError{Moderation: user.Moderation})
		return
	}
	writeSubscriptionData(w, auth.DirectoryView(user))
}

func (s trustedAuthAPI) changePassword(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Email string `json:"email"`
		Old   string `json:"oldPassword"`
		New   string `json:"newPassword"`
	}
	if !decodeEmailRequest(w, r, &input) || !validRustPassword(w, input.Old) || !validRustPassword(w, input.New) {
		return
	}
	user, err := s.backend.VerifyPassword(r.Context(), input.Email, input.Old)
	if err == nil && user.Moderation.Banned() {
		err = auth.BannedError{Moderation: user.Moderation}
	}
	if err == nil {
		err = s.accounts.ChangePassword(r.Context(), user.ID, input.Old, input.New)
	}
	if err != nil {
		trustedAuthError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "200"})
}

func (s trustedAuthAPI) verifyGoogle(w http.ResponseWriter, r *http.Request) {
	var input struct{ Email, Token, Code, Language string }
	if !decodeEmailRequest(w, r, &input) {
		return
	}
	user, created, err := s.google.VerifyIdentity(r.Context(), input.Token, input.Code, input.Language, input.Email)
	if err != nil {
		trustedAuthError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "user": auth.DirectoryView(user), "isNew": created})
}

func (s trustedAuthAPI) exchangeCode(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ID          subscriptionID `json:"userId"`
		AccessToken string         `json:"accessToken"`
	}
	if !decodeEmailRequest(w, r, &input) {
		return
	}
	if input.ID < 1 || input.AccessToken == "" {
		trustedAuthError(w, auth.ErrInvalidCredentials)
		return
	}
	binding := auth.GrantBinding{Purpose: auth.ServiceCode, ClientID: auth.TrustedUserService, UserID: int64(input.ID)}
	code, err := auth.IssueTrustedGrant(r.Context(), s.accounts, s.grants, input.AccessToken, binding)
	if err != nil {
		trustedAuthError(w, err)
		return
	}
	writeSubscriptionData(w, map[string]string{"code": code})
}

func (s trustedAuthAPI) verifyCode(w http.ResponseWriter, r *http.Request) {
	var input struct{ Code string }
	if !decodeEmailRequest(w, r, &input) {
		return
	}
	binding := auth.GrantBinding{Purpose: auth.ServiceCode, ClientID: auth.TrustedUserService}
	user, err := auth.VerifyTrustedGrant(r.Context(), s.accounts, s.grants, input.Code, binding)
	if err != nil {
		trustedAuthError(w, err)
		return
	}
	writeSubscriptionData(w, auth.DirectoryView(user))
}

func trustedAuthError(w http.ResponseWriter, err error) {
	if writeBanned(w, err, true) {
		return
	}
	switch {
	case errors.Is(err, auth.ErrDirectoryUserNotFound):
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "4005", "message": "user not found"})
	case errors.Is(err, auth.ErrInvalidCredentials), errors.Is(err, auth.ErrTrustedGrant),
		errors.Is(err, auth.ErrGoogleToken), errors.Is(err, auth.ErrInvalidEmailCode):
		httpx.JSON(w, http.StatusUnauthorized, map[string]string{"status": "401", "message": "invalid or expired authorization"})
	case errors.Is(err, auth.ErrGoogleMailboxProof):
		httpx.Error(w, http.StatusForbidden, "email ownership could not be verified")
	case errors.Is(err, auth.ErrExchangeUnavailable), errors.Is(err, auth.ErrGoogleUnavailable):
		httpx.Error(w, http.StatusServiceUnavailable, "trusted authorization is unavailable")
	default:
		httpx.Error(w, http.StatusInternalServerError, "could not authorize user service")
	}
}
