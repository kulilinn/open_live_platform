package httpapi

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"open_live_platform/internal/live"
	"open_live_platform/internal/posts"
	"open_live_platform/internal/transport/httpx"
)

type roomSettingsInput struct {
	Title               *string   `json:"title"`
	Subject             *string   `json:"subject"`
	Password            *string   `json:"password"`
	Banner              *string   `json:"banner"`
	Tag                 *string   `json:"tag"`
	Description         *string   `json:"description"`
	OfflineInfo         *string   `json:"offlineInfo"`
	Plan                *[]string `json:"plan"`
	Gifts               *[]string `json:"gifts"`
	TranslationEnabled  *bool     `json:"trans"`
	TranslationLanguage *string   `json:"transLang"`
}

func (input roomSettingsInput) settings() live.RoomSettings {
	return live.RoomSettings{
		Title: cmp.Or(input.Title, input.Subject), Password: input.Password, Banner: input.Banner,
		Tag: input.Tag, Description: input.Description, OfflineInfo: input.OfflineInfo,
		Plan: input.Plan, Gifts: input.Gifts, TranslationEnabled: input.TranslationEnabled,
		TranslationLanguage: input.TranslationLanguage,
	}
}

func (s server) startRoom(w http.ResponseWriter, r *http.Request, userID int64) {
	var input *roomSettingsInput
	if !httpx.Decode(w, r, &input) {
		return
	}
	if input == nil {
		httpx.Error(w, http.StatusBadRequest, "expected a room settings object")
		return
	}
	rust := rustRoomCompat(r) || (input.Title == nil && (input.Subject != nil || input.Banner != nil || input.Tag != nil ||
		input.Description != nil || input.OfflineInfo != nil || input.Plan != nil || input.Gifts != nil || input.TranslationEnabled != nil))
	settings := input.settings()
	ctx, ok := s.auditRoomFields(w, r.Context(), userID, settings, true)
	if !ok {
		return
	}
	room, err := s.rooms.StartWithSettings(ctx, userID, settings, rust)
	if err != nil {
		if rust && errors.Is(err, live.ErrRoomLimited) {
			httpx.JSON(w, http.StatusOK, map[string]string{"status": "4035", "message": "room is limited"})
			return
		}
		if rust && errors.Is(err, live.ErrRoomAlreadyOnline) {
			httpx.JSON(w, http.StatusOK, map[string]string{"status": "4079", "message": "live stream already start"})
			return
		}
		httpx.LiveError(w, err)
		return
	}
	if room.TranslationEnabled && s.subtitles != nil && s.subtitles.Enabled() {
		language := cmp.Or(r.Header.Get(sttLanguageHeader), room.TranslationLanguage)
		if language != "" {
			if err := s.subtitles.Start(r.Context(), room.ID, userID, []string{language}, true); err != nil {
				log.Printf("start Agora STT for room %d: %v", room.ID, err)
			}
		}
	}
	if rust {
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
	preferences, err := s.accounts.TranslationLanguages(r.Context(), []int64{userID})
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "could not read room translation language")
		return
	}
	joined, err := s.rooms.JoinRoomWithLanguage(r.Context(), roomID, userID, input.Password, preferences[userID])
	if !rustRoomCompat(r) {
		liveResult(w, err)
		return
	}
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
	languages := joined.Languages
	if languages == nil {
		languages = []string{}
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

func (s server) roomToken(w http.ResponseWriter, r *http.Request, userID int64) {
	roomID, ok := httpx.PathID(w, r, "channel")
	if !ok {
		return
	}
	role := r.PathValue("role")
	if s.dates != nil && s.dates.roomToken(w, r, roomID, userID, role) {
		return
	}
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
	var input *roomSettingsInput
	if !httpx.Decode(w, r, &input) {
		return
	}
	if input == nil {
		httpx.Error(w, http.StatusBadRequest, "expected a room settings object")
		return
	}
	settings := input.settings()
	ctx, ok := s.auditRoomFields(w, r.Context(), userID, settings, false)
	if !ok {
		return
	}
	rust := rustRoomCompat(r)
	var room live.Room
	var err error
	if rust {
		settings.Password = cmp.Or(settings.Password, new(string))
		room, err = s.rooms.ConfigureRoom(ctx, userID, settings)
	} else {
		room, err = s.rooms.UpdateSettings(ctx, userID, settings)
	}
	if err != nil {
		httpx.LiveError(w, err)
		return
	}
	var info map[string]any
	var infoErr error
	if rust {
		info, infoErr = s.roomSettingsInfo(r.Context(), room)
	} else {
		info, infoErr = s.roomInfo(r.Context(), userID, room)
	}
	if infoErr != nil {
		httpx.Error(w, http.StatusInternalServerError, "could not build room response")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": info})
}

func (s server) auditRoomFields(w http.ResponseWriter, ctx context.Context, owner int64, settings live.RoomSettings, start bool) (context.Context, bool) {
	if s.staticReviewer == nil || (settings.Title == nil && settings.Banner == nil && settings.OfflineInfo == nil) {
		return ctx, true
	}
	room, err := s.rooms.Info(ctx, owner)
	if err != nil && !errors.Is(err, live.ErrRoomNotFound) {
		httpx.LiveError(w, err)
		return ctx, false
	}
	if start && room.Online {
		return ctx, true
	}
	return submitStaticReviews(w, ctx, s.staticReviewer, owner,
		staticContent{typeID: posts.ReviewRoomTitle, object: owner, kind: "room_title", before: room.Title, after: optionalStaticValue(settings.Title), refusal: "4064"},
		staticContent{typeID: posts.ReviewRoomBanner, object: owner, kind: "room_banner", before: room.Banner, after: optionalStaticValue(settings.Banner), refusal: "4063", image: true},
		staticContent{typeID: posts.ReviewOfflineInfo, object: owner, kind: "room_offline_info", before: room.OfflineInfo, after: optionalStaticValue(settings.OfflineInfo), refusal: "4057"})
}

func (s server) listRooms(w http.ResponseWriter, r *http.Request, userID int64) {
	var input struct {
		Page     *int64  `json:"page"`
		PageSize *int64  `json:"pageSize"`
		Online   *bool   `json:"online"`
		Tag      *string `json:"tag"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil && err != io.EOF {
		httpx.Error(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	// Older clients send an empty request and expect a plain array.
	rust := rustRoomCompat(r) || input.Tag != nil
	if !rust && input.Page == nil && input.PageSize == nil && input.Online == nil {
		rooms, err := s.rooms.Rooms(r.Context())
		if err != nil {
			httpx.LiveError(w, err)
			return
		}
		httpx.JSON(w, http.StatusOK, rooms)
		return
	}
	filter := live.RoomFilter{Online: input.Online, Tag: input.Tag, Page: 1, PageSize: 20}
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
	if rust {
		owners := make([]int64, 0, len(rooms))
		for _, room := range rooms {
			owners = append(owners, room.OwnerID)
		}
		names, err := s.accounts.Names(r.Context(), owners)
		if err != nil {
			httpx.Error(w, http.StatusInternalServerError, "could not build room list")
			return
		}
		data := make([]map[string]any, 0, len(rooms))
		for _, room := range rooms {
			info := roomInfoFields(userID, room, names[room.OwnerID], nil, -1, false)
			info["memberCount"] = 0
			info["member_count"] = 0
			data = append(data, info)
		}
		httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": data, "pages": pages})
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
			err := action(r.Context(), roomID, userID, targetID)
			if rustRoomCompat(r) && errors.Is(err, live.ErrRoomNotFound) {
				roomStatusError(w, err)
				return
			}
			liveResult(w, err)
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
	message := err.Error()
	switch {
	case fallback == "4045" && (errors.Is(err, live.ErrRoomNotFound) || errors.Is(err, live.ErrRoomClosed)):
		message = "leave failed"
	case errors.Is(err, live.ErrInvalidRoomPassword):
		status = "4083"
	case errors.Is(err, live.ErrRoomPasswordLocked):
		status = "4084"
	case errors.Is(err, live.ErrRoomNotFound) && fallback != "4045":
		status = "4005"
		message = "data not found "
	case errors.Is(err, live.ErrRoomClosed) && fallback != "4045":
		status = "4005"
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"status": status, "message": message})
}
