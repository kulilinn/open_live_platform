package httpx

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strconv"

	"open_live_platform/internal/live"
)

func JSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}

func Error(w http.ResponseWriter, status int, message string) {
	JSON(w, status, map[string]string{"error": message})
}

func Decode(w http.ResponseWriter, r *http.Request, value any) bool {
	if err := json.NewDecoder(r.Body).Decode(value); err != nil {
		Error(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

func RequireToken(w http.ResponseWriter, r *http.Request, token string) bool {
	if token == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
		w.Header().Set("WWW-Authenticate", "Bearer")
		Error(w, http.StatusUnauthorized, "invalid bearer token")
		return false
	}
	return true
}

func PathID(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || id < 1 {
		Error(w, http.StatusBadRequest, "invalid "+name+" ID")
		return 0, false
	}
	return id, true
}

func LiveError(w http.ResponseWriter, err error) {
	switch err {
	case live.ErrRoomNotFound:
		Error(w, http.StatusNotFound, err.Error())
	case live.ErrRoomClosed, live.ErrRoomAlreadyOnline:
		Error(w, http.StatusConflict, err.Error())
	case live.ErrForbidden, live.ErrNotMember, live.ErrMuted, live.ErrKicked, live.ErrInvalidRoomPassword:
		Error(w, http.StatusForbidden, err.Error())
	case live.ErrRoomPasswordLocked:
		Error(w, http.StatusTooManyRequests, err.Error())
	default:
		Error(w, http.StatusInternalServerError, "room operation failed")
	}
}
