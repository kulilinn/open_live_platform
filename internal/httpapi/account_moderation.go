package httpapi

import (
	"errors"
	"net/http"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/transport/httpx"
)

func writeBanned(w http.ResponseWriter, err error, rust bool) bool {
	var banned auth.BannedError
	if !errors.As(err, &banned) {
		return false
	}
	w.Header().Set("Cache-Control", "no-store")
	if !rust {
		httpx.Error(w, http.StatusForbidden, banned.Error())
		return true
	}
	m := banned.Moderation
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "4047", "data": map[string]any{
		"limit_total": m.Total, "limit_duration": m.Until, "limit_reason": m.Reasons(),
	}})
	return true
}
