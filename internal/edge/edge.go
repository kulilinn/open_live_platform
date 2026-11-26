package edge

import (
	"net/http"
	"time"

	"open_live_platform/internal/live"
	"open_live_platform/internal/notice"
	"open_live_platform/internal/transport/httpx"
)

func Register(mux *http.ServeMux, rooms live.RoomStore, notices notice.Store, token string) {
	mux.HandleFunc("POST /api/edge/events", func(w http.ResponseWriter, r *http.Request) {
		if !httpx.RequireToken(w, r, token) {
			return
		}
		var event struct {
			live.Event
			Notice notice.Notice `json:"notice"`
		}
		if !httpx.Decode(w, r, &event) {
			return
		}
		if event.Type == "notice" {
			n := event.Notice
			if n.ID < 1 || n.UserID < 1 || n.Type < 1 || n.Type > 34 || n.Target < 1 || n.Target > 3 || n.Content == nil {
				httpx.Error(w, http.StatusBadRequest, "invalid notice")
				return
			}
			if n.CreatedAt.IsZero() {
				n.CreatedAt = time.Now().UTC()
			}
			if err := notices.AddNotice(r.Context(), n); err != nil {
				httpx.Error(w, http.StatusInternalServerError, "could not store notice")
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if event.Type != "chat" {
			httpx.Error(w, http.StatusBadRequest, "unsupported edge event type")
			return
		}
		if event.Text == "" {
			httpx.Error(w, http.StatusBadRequest, "text is required")
			return
		}
		if err := rooms.Send(r.Context(), event.RoomID, event.UserID, event.Text); err != nil {
			httpx.LiveError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
