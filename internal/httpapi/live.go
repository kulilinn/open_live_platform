package httpapi

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"open_live_platform/internal/live"
	"open_live_platform/internal/transport/httpx"
)

func (s server) startRoom(w http.ResponseWriter, r *http.Request, userID int64) {
	var input struct {
		Title    *string `json:"title"`
		Subject  *string `json:"subject"`
		Password string  `json:"password"`
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
	room, err := s.rooms.Start(r.Context(), userID, title, input.Password)
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
	room, err := s.rooms.JoinRoom(r.Context(), roomID, userID, input.Password)
	if err != nil {
		rustRoomError(w, err, "4005")
		return
	}
	info, err := s.roomInfo(r.Context(), userID, room)
	if err != nil {
		rustRoomError(w, err, "4005")
		return
	}
	nid := strconv.FormatInt(userID, 10)
	isAnchor := userID == room.OwnerID
	if isAnchor {
		nid = strconv.FormatUint(uint64(room.Session.AnchorNID), 10)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"status": "200",
		"data": map[string]any{
			"room":      info,
			"languages": []string{},
			"user":      map[string]any{"id": userID},
			"rtcToken": map[string]any{
				"appid": "", "channel": strconv.FormatInt(roomID, 10),
				"uid": strconv.FormatInt(userID, 10), "nid": nid,
				"token": "", "isAnchor": isAnchor,
			},
			"startTime": room.Session.StartedAt.Unix(),
		},
	})
}

func (s server) updateRoomSettings(w http.ResponseWriter, r *http.Request, userID int64) {
	var input *struct {
		Title    *string `json:"title"`
		Subject  *string `json:"subject"`
		Password *string `json:"password"`
	}
	if !httpx.Decode(w, r, &input) {
		return
	}
	if input == nil {
		httpx.Error(w, http.StatusBadRequest, "expected a room settings object")
		return
	}
	room, err := s.rooms.UpdateSettings(r.Context(), userID, cmp.Or(input.Title, input.Subject), input.Password)
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
