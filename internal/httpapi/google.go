package httpapi

import (
	"cmp"
	"errors"
	"net/http"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/transport/httpx"
)

func RegisterGoogle(mux *http.ServeMux, service auth.GoogleLogin) {
	mux.HandleFunc("POST /api/auth/verifygoogle", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Token, Code string }
		if !decodeEmailRequest(w, r, &body) {
			return
		}
		language := cmp.Or(r.Header.Get("x-loverose-lang"), "ja")
		session, created, err := service.Login(r.Context(), body.Token, body.Code, language)
		if writeBanned(w, err, true) {
			return
		}
		switch {
		case errors.Is(err, auth.ErrGoogleUnavailable):
			httpx.Error(w, 503, err.Error())
		case errors.Is(err, auth.ErrGoogleMailboxProof):
			httpx.Error(w, 403, err.Error())
		case errors.Is(err, auth.ErrInvalidEmailCode), errors.Is(err, auth.ErrGoogleToken):
			httpx.Error(w, 401, "invalid login credentials")
		case err != nil:
			httpx.Error(w, 500, "could not sign in")
		default:
			u := rustAuthUser(session.User)
			u["isNew"], u["language"] = created, language
			w.Header().Set("Cache-Control", "no-store")
			httpx.JSON(w, 200, map[string]any{"status": "200", "user": u, "token": session.Token})
		}
	})
}
