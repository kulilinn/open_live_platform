package edge

import (
	"net/http"

	"open_live_platform/internal/live"
	"open_live_platform/internal/transport/httpx"
)

func Register(mux *http.ServeMux, rooms live.RoomStore, token string) {
	mux.HandleFunc("POST /api/edge/events", func(w http.ResponseWriter, r *http.Request) {
		if !httpx.RequireToken(w, r, token) {
			return
		}
		var event live.Event
		if !httpx.Decode(w, r, &event) {
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
