package httpapi

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"open_live_platform/internal/live"
	"open_live_platform/internal/transport/httpx"
)

func (s server) startRoom(w http.ResponseWriter, r *http.Request, userID int64) {
	var input struct {
		Title               *string `json:"title"`
		Subject             *string `json:"subject"`
		Password            string  `json:"password"`
		TranslationLanguage *string `json:"transLang"`
	}
	if !httpx.Decode(w, r, &input) {
		return
	}
	title := ""
	if input.Title != nil {
		title = *input.Title
	} else if input.Subject != nil {
		title = *input.Subject
	}
	room, err := s.rooms.StartWithLanguage(r.Context(), userID, title, input.Password, input.TranslationLanguage)
	if err != nil {
		httpx.LiveError(w, err)
		return
	}
	if input.Subject != nil && input.Title == nil {
		info, infoErr := s.roomInfo(r.Context(), userID, room)
		if infoErr != nil {
			httpx.Error(w, http.StatusInternalServerError, "could not build room response")
			return
		}
		response := legacyRoomJSON(room)
		response["status"] = "200"
		response["data"] = info
		httpx.JSON(w, http.StatusOK, response)
		return
	}
	httpx.JSON(w, http.StatusOK, room)
}

func (s server) joinRoom(w http.ResponseWriter, r *http.Request, userID int64) {
	roomID, ok := httpx.PathID(w, r, "room")
	if !ok {
		return
	}
	var input struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil && err != io.EOF {
		httpx.Error(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if !rustRoomCompat(r) {
		liveResult(w, s.rooms.Join(r.Context(), roomID, userID, input.Password))
		return
	}
	joined, err := s.rooms.JoinRoom(r.Context(), roomID, userID, input.Password)
	if err != nil {
		rustRoomError(w, err, "4005")
		return
	}
	room := joined.Room
	info, err := s.roomInfoWithMembers(r.Context(), userID, room, joined.Members)
	if err != nil {
		rustRoomError(w, err, "4005")
		return
	}
	languages, err := s.roomLanguages(r.Context(), joined)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "could not read room translation languages")
		return
	}
	token, err := s.rtc.ForRoom(roomID, userID, room.ViewerNID, userID == room.OwnerID)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "could not issue RTC token")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, map[string]any{
		"status": "200",
		"data": map[string]any{
			"room":      info,
			"languages": languages,
			"user":      map[string]any{"id": userID},
			"rtcToken":  token,
			"startTime": room.Session.StartedAt.Unix(),
		},
	})
}

func (s server) roomLanguages(ctx context.Context, room live.JoinedRoom) ([]string, error) {
	translations, err := s.accounts.TranslationLanguages(ctx, room.Members)
	if err != nil {
		return nil, err
	}
	languages := make(map[string]struct{})
	for _, id := range room.Members {
		if id != room.OwnerID {
			languages[cmp.Or(translations[id], "en")] = struct{}{}
		}
	}
	if room.AnchorJoined {
		languages[cmp.Or(room.TranslationLanguage, "en")] = struct{}{}
	}
	return slices.Sorted(maps.Keys(languages)), nil
}

func (s server) roomToken(w http.ResponseWriter, r *http.Request, userID int64) {
	roomID, ok := httpx.PathID(w, r, "channel")
	if !ok {
		return
	}
	role := r.PathValue("role")
	if role != "2" && (role != "1" || roomID != userID) {
		httpx.LiveError(w, live.ErrForbidden)
		return
	}
	nid, err := s.rooms.RTCNID(r.Context(), roomID, userID)
	if err != nil {
		httpx.LiveError(w, err)
		return
	}
	token, err := s.rtc.ForRoom(roomID, userID, nid, role == "1")
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "could not issue RTC token")
		return
	}
	if token.Token == "" {
		httpx.Error(w, http.StatusServiceUnavailable, "RTC signing is not configured")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": token})
}

func (s server) updateRoomSettings(w http.ResponseWriter, r *http.Request, userID int64) {
	var input *struct {
		Title               *string `json:"title"`
		Subject             *string `json:"subject"`
		Password            *string `json:"password"`
		TranslationLanguage *string `json:"transLang"`
	}
	if !httpx.Decode(w, r, &input) {
		return
	}
	if input == nil {
		httpx.Error(w, http.StatusBadRequest, "expected a room settings object")
		return
	}
	room, err := s.rooms.UpdateSettings(r.Context(), userID, live.RoomSettings{
		Title: cmp.Or(input.Title, input.Subject), Password: input.Password, TranslationLanguage: input.TranslationLanguage,
	})
	if err != nil {
		httpx.LiveError(w, err)
		return
	}
	info, infoErr := s.roomInfo(r.Context(), userID, room)
	if infoErr != nil {
		httpx.Error(w, http.StatusInternalServerError, "could not build room response")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": info})
}

func (s server) listRooms(w http.ResponseWriter, r *http.Request, _ int64) {
	var input struct {
		Page     *int64 `json:"page"`
		PageSize *int64 `json:"pageSize"`
		Online   *bool  `json:"online"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil && err != io.EOF {
		httpx.Error(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	// Older clients send an empty request and expect a plain array.
	if input.Page == nil && input.PageSize == nil && input.Online == nil {
		rooms, err := s.rooms.Rooms(r.Context())
		if err != nil {
			httpx.LiveError(w, err)
			return
		}
		httpx.JSON(w, http.StatusOK, rooms)
		return
	}
	filter := live.RoomFilter{Online: input.Online, Page: 1, PageSize: 20}
	if input.Page != nil {
		filter.Page = *input.Page
	}
	if input.PageSize != nil {
		filter.PageSize = *input.PageSize
	}
	if filter.Page < 1 || filter.PageSize < 0 {
		httpx.Error(w, http.StatusBadRequest, "page must be positive and pageSize must be non-negative")
		return
	}
	rooms, pages, err := s.rooms.ListRooms(r.Context(), filter)
	if err != nil {
		httpx.LiveError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": rooms, "pages": pages})
}

func (s server) setAdmin(w http.ResponseWriter, r *http.Request, userID int64) {
	targetAction(func(ctx context.Context, roomID, actorID, targetID int64) error {
		return s.rooms.SetAdmin(ctx, roomID, actorID, targetID, r.Method == http.MethodPost)
	})(w, r, userID)
}

func (s server) setMute(w http.ResponseWriter, r *http.Request, userID int64) {
	var duration time.Duration
	if r.Method == http.MethodPost {
		duration = 1<<63 - 1 // The room store resets mutes on the next broadcast.
	}
	targetAction(func(ctx context.Context, roomID, actorID, targetID int64) error {
		return s.rooms.Mute(ctx, roomID, actorID, targetID, duration)
	})(w, r, userID)
}

func roomQuery[T any](query func(context.Context, int64) (T, error)) userHandler {
	return func(w http.ResponseWriter, r *http.Request, _ int64) {
		roomID, ok := httpx.PathID(w, r, "room")
		if !ok {
			return
		}
		result, err := query(r.Context(), roomID)
		if err != nil {
			httpx.LiveError(w, err)
			return
		}
		httpx.JSON(w, http.StatusOK, result)
	}
}

func roomAction(action func(context.Context, int64, int64) error) userHandler {
	return func(w http.ResponseWriter, r *http.Request, userID int64) {
		roomID, ok := httpx.PathID(w, r, "room")
		if ok {
			err := action(r.Context(), roomID, userID)
			if rustRoomCompat(r) {
				if err != nil {
					rustRoomError(w, err, "4045")
					return
				}
				httpx.JSON(w, http.StatusOK, map[string]string{"status": "200"})
				return
			}
			liveResult(w, err)
		}
	}
}

func targetAction(action func(context.Context, int64, int64, int64) error) userHandler {
	return func(w http.ResponseWriter, r *http.Request, userID int64) {
		roomID, ok := httpx.PathID(w, r, "room")
		if !ok {
			return
		}
		targetID, ok := httpx.PathID(w, r, "uid")
		if ok {
			liveResult(w, action(r.Context(), roomID, userID, targetID))
		}
	}
}

func liveResult(w http.ResponseWriter, err error) {
	if err != nil {
		httpx.LiveError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func rustRoomCompat(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/vnd.livemedia+json") ||
		r.URL.Query().Get("compat") == "rust"
}

func rustRoomError(w http.ResponseWriter, err error, fallback string) {
	status := fallback
	switch {
	case errors.Is(err, live.ErrInvalidRoomPassword):
		status = "4083"
	case errors.Is(err, live.ErrRoomPasswordLocked):
		status = "4084"
	case errors.Is(err, live.ErrRoomNotFound), errors.Is(err, live.ErrRoomClosed):
		status = "4005"
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"status": status, "message": err.Error()})
}
