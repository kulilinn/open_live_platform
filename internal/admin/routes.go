package admin

import (
	"net/http"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/live"
	"open_live_platform/internal/transport/httpx"
)

func Register(mux *http.ServeMux, accounts auth.AccountStore, rooms live.RoomStore, token string) {
	add := func(pattern string, handler http.HandlerFunc) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if httpx.RequireToken(w, r, token) {
				handler(w, r)
			}
		})
	}
	add("GET /api/admin/rooms", func(w http.ResponseWriter, r *http.Request) {
		list, err := rooms.Rooms(r.Context())
		if err != nil {
			httpx.LiveError(w, err)
			return
		}
		httpx.JSON(w, http.StatusOK, list)
	})
	add("GET /api/admin/rooms/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := httpx.PathID(w, r, "id")
		if !ok {
			return
		}
		room, err := rooms.Info(r.Context(), id)
		if err != nil {
			httpx.LiveError(w, err)
			return
		}
		httpx.JSON(w, http.StatusOK, room)
	})
	add("GET /api/admin/users", func(w http.ResponseWriter, r *http.Request) {
		list, err := accounts.Users(r.Context())
		if err != nil {
			httpx.Error(w, http.StatusInternalServerError, "could not list users")
			return
		}
		httpx.JSON(w, http.StatusOK, list)
	})
	add("GET /api/admin/users/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := httpx.PathID(w, r, "id")
		if !ok {
			return
		}
		list, err := accounts.Users(r.Context())
		if err != nil {
			httpx.Error(w, http.StatusInternalServerError, "could not list users")
			return
		}
		for _, user := range list {
			if user.ID == id {
				httpx.JSON(w, http.StatusOK, user)
				return
			}
		}
		httpx.Error(w, http.StatusNotFound, "user not found")
	})
}
