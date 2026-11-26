package httpapi

import (
	"errors"
	"net/http"
	"time"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/transport/httpx"
)

type emailCodeInput struct {
	Email    string `json:"email"`
	Code     string `json:"code"`
	Password string `json:"password"`
}

func (s server) emailCode(w http.ResponseWriter, r *http.Request) {
	if !s.requireEmailVerification(w) {
		return
	}
	var input struct {
		Email    string `json:"email"`
		Signin   *bool  `json:"signin"`
		SendMail *bool  `json:"sendMail"`
		Code     string `json:"code"`
	}
	if !decodeEmailRequest(w, r, &input) {
		return
	}
	if input.Signin == nil || (input.SendMail != nil && !*input.SendMail) || input.Code != "" {
		httpx.Error(w, http.StatusBadRequest, "signin is required and the server sends the verification code")
		return
	}
	offer, err := s.emailVerification.Issue(r.Context(), input.Email, *input.Signin)
	if err != nil {
		emailCodeError(w, err, false)
		return
	}
	seconds := max(0, (time.Until(offer.ExpiresAt)+time.Second-1)/time.Second)
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "timeleft": int64(seconds), "needSend": offer.New})
}

func (s server) verifyEmailCode(w http.ResponseWriter, r *http.Request) {
	if !s.requireEmailVerification(w) {
		return
	}
	var input emailCodeInput
	if !decodeEmailRequest(w, r, &input) {
		return
	}
	session, err := s.emailVerification.Login(r.Context(), input.Email, input.Code)
	if err != nil {
		emailCodeError(w, err, false)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "200", "token": session.Token})
}

func (s server) resetPassword(w http.ResponseWriter, r *http.Request) {
	if !s.requireEmailVerification(w) {
		return
	}
	var input emailCodeInput
	if !decodeEmailRequest(w, r, &input) {
		return
	}
	if !validRustPassword(w, input.Password) {
		return
	}
	if err := s.emailVerification.Reset(r.Context(), input.Email, input.Code, input.Password); err != nil {
		emailCodeError(w, err, true)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "200"})
}

func (s server) requireEmailVerification(w http.ResponseWriter) bool {
	if s.emailVerification != nil {
		return true
	}
	httpx.Error(w, http.StatusServiceUnavailable, "email verification is unavailable")
	return false
}

func decodeEmailRequest(w http.ResponseWriter, r *http.Request, value any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 8*1024)
	return httpx.Decode(w, r, value)
}

func validRustPassword(w http.ResponseWriter, value string) bool {
	if len(value) >= 8 && len(value) <= 20 {
		return true
	}
	httpx.JSON(w, http.StatusBadRequest, map[string]string{"status": "4071", "message": "password length must be 8 to 20 bytes"})
	return false
}

func emailCodeError(w http.ResponseWriter, err error, rustCodeError bool) {
	switch {
	case errors.Is(err, auth.ErrInvalidEmailCode) && rustCodeError:
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "4013", "message": err.Error()})
	case errors.Is(err, auth.ErrInvalidEmailCode):
		httpx.Error(w, http.StatusUnauthorized, err.Error())
	case errors.Is(err, auth.ErrInvalidEmail), errors.Is(err, auth.ErrMissingFields):
		httpx.Error(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, auth.ErrEmailNotFound):
		httpx.Error(w, http.StatusNotFound, err.Error())
	case errors.Is(err, auth.ErrEmailInUse):
		httpx.Error(w, http.StatusConflict, err.Error())
	case errors.Is(err, auth.ErrEmailCodeDelivery):
		httpx.Error(w, http.StatusServiceUnavailable, err.Error())
	default:
		httpx.Error(w, http.StatusInternalServerError, "could not verify email")
	}
}
