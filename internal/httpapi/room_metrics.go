package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/live"
	"open_live_platform/internal/transport/httpx"
)

func (s server) roomAnchor(w http.ResponseWriter, r *http.Request, _ int64) {
	roomID, ok := httpx.PathID(w, r, "room")
	if !ok {
		return
	}
	room, err := s.rooms.Info(r.Context(), roomID)
	if err != nil {
		roomStatusError(w, err)
		return
	}
	user, ok, err := s.accounts.Find(r.Context(), room.OwnerID)
	if err != nil || !ok {
		if err == nil {
			err = live.ErrRoomNotFound
		}
		roomStatusError(w, err)
		return
	}
	profile := auth.PublicProfile(user)
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": map[string]any{
		"id": strconv.FormatInt(user.ID, 10), "nid": strconv.FormatUint(uint64(live.DefaultAnchorNID), 10),
		"searchName": profile.SearchName, "displayName": user.Account, "avatar": profile.Photo, "level": 0, "country": "",
	}})
}

func (s server) registerRoomMetrics(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/live/room/anchor/{room}", s.authenticated(s.roomAnchor))
	mux.HandleFunc("POST /api/live/room/sync_watch_time", s.authenticated(func(w http.ResponseWriter, _ *http.Request, _ int64) {
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "200"})
	}))
	metrics, ok := s.rooms.(live.RoomMetricsStore)
	if !ok {
		return
	}
	mux.HandleFunc("POST /api/live/room/live/like", s.authenticated(func(w http.ResponseWriter, r *http.Request, uid int64) {
		var input struct {
			RoomID int64 `json:"roomId,string"`
			LiveID int64 `json:"liveId,string"`
			Count  int64 `json:"likeCount"`
		}
		if !httpx.Decode(w, r, &input) {
			return
		}
		if input.RoomID <= 0 || input.LiveID <= 0 || input.Count <= 0 {
			httpx.Error(w, http.StatusBadRequest, "invalid room, broadcast or like count")
			return
		}
		count, err := metrics.AddRoomLikes(r.Context(), input.RoomID, input.LiveID, uid, input.Count)
		switch {
		case errors.Is(err, live.ErrRoomNotFound):
			httpx.JSON(w, http.StatusOK, map[string]string{"status": "404", "message": "Chat room not found"})
		case errors.Is(err, live.ErrLikeCount):
			httpx.Error(w, http.StatusBadRequest, err.Error())
		case err != nil:
			httpx.LiveError(w, err)
		default:
			httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": count})
		}
	}))
	mux.HandleFunc("GET /api/live/room/settle/{live_id}", s.authenticated(func(w http.ResponseWriter, r *http.Request, uid int64) {
		liveID, ok := httpx.PathID(w, r, "live_id")
		if !ok {
			return
		}
		data, err := metrics.RoomSettlement(r.Context(), liveID, uid)
		if err != nil {
			roomStatusError(w, err)
			return
		}
		httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": data})
	}))
	mux.HandleFunc("DELETE /api/live/room/report/{room}", s.authenticated(func(w http.ResponseWriter, r *http.Request, uid int64) {
		report := live.RoomReport{Room: r.PathValue("room"), ReporterID: uid}
		if !httpx.Decode(w, r, &report) {
			return
		}
		if utf8.RuneCountInString(report.Subject) > 256 || utf8.RuneCountInString(report.Description) > 4096 ||
			strings.ContainsRune(report.Subject, 0) || strings.ContainsRune(report.Description, 0) {
			httpx.Error(w, http.StatusBadRequest, "invalid room report")
			return
		}
		if err := metrics.ReportRoom(r.Context(), report); err != nil {
			httpx.Error(w, http.StatusInternalServerError, "could not save room report")
			return
		}
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "200"})
	}))
}
