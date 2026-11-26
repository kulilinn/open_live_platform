package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"open_live_platform/internal/live"
	"open_live_platform/internal/transport/httpx"
)

func (s server) kicked(w http.ResponseWriter, r *http.Request, _ int64) {
	roomID, userID, ok := twoPathIDs(w, r)
	if !ok {
		return
	}
	value, err := s.rooms.Kicked(r.Context(), roomID, userID)
	if err != nil {
		roomStatusError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": value})
}

func (s server) banned(w http.ResponseWriter, r *http.Request, _ int64) {
	roomID, userID, ok := twoPathIDs(w, r)
	if !ok {
		return
	}
	value, err := s.rooms.Muted(r.Context(), roomID, userID)
	if err != nil {
		roomStatusError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": value})
}

func (s server) limited(w http.ResponseWriter, r *http.Request, _ int64) {
	roomID, ok := httpx.PathID(w, r, "room")
	if !ok {
		return
	}
	if _, err := s.rooms.Info(r.Context(), roomID); err != nil {
		roomStatusError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": map[string]any{
		"limit_ty": 0, "limit_reason": []string{}, "limit_allow": true,
		"limit_duration": 0, "limit_total": 0,
	}})
}

func (s server) checkPassword(w http.ResponseWriter, r *http.Request, _ int64) {
	roomID, ok := httpx.PathID(w, r, "room_id")
	if !ok {
		return
	}
	room, err := s.rooms.Info(r.Context(), roomID)
	if err != nil {
		roomStatusError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"status": "200", "data": map[string]any{"isLock": room.IsLock},
	})
}

func (s server) online(w http.ResponseWriter, r *http.Request, userID int64) {
	room, err := s.rooms.Info(r.Context(), userID)
	if err != nil {
		roomStatusError(w, err)
		return
	}
	online := 0
	if room.Online {
		online = 1
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"status": "200", "data": map[string]any{"online": online, "duration": int64(0)},
	})
}

func (s server) audience(w http.ResponseWriter, r *http.Request, _ int64) {
	roomID, userID, ok := twoPathIDs(w, r)
	if !ok {
		return
	}
	if _, err := s.rooms.Info(r.Context(), roomID); err != nil {
		roomStatusError(w, err)
		return
	}
	admins, err := s.rooms.Admins(r.Context(), roomID)
	if err != nil {
		roomStatusError(w, err)
		return
	}
	muted, err := s.rooms.MutedUsers(r.Context(), roomID)
	if err != nil {
		roomStatusError(w, err)
		return
	}
	data, err := s.audienceData(r.Context(), []int64{userID}, admins, muted)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "could not read audience")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": data[0]})
}

func (s server) admins(w http.ResponseWriter, r *http.Request, _ int64) {
	roomID, ok := httpx.PathID(w, r, "room")
	if !ok {
		return
	}
	if _, err := s.rooms.Info(r.Context(), roomID); err != nil {
		roomStatusError(w, err)
		return
	}
	admins, err := s.rooms.Admins(r.Context(), roomID)
	if err != nil {
		roomStatusError(w, err)
		return
	}
	muted, err := s.rooms.MutedUsers(r.Context(), roomID)
	if err != nil {
		roomStatusError(w, err)
		return
	}
	data, err := s.audienceData(r.Context(), admins, admins, muted)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "could not read administrators")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": data})
}

func (s server) bans(w http.ResponseWriter, r *http.Request, _ int64) {
	roomID, ok := httpx.PathID(w, r, "room")
	if !ok {
		return
	}
	if _, err := s.rooms.Info(r.Context(), roomID); err != nil {
		roomStatusError(w, err)
		return
	}
	banned, err := s.rooms.MutedUsers(r.Context(), roomID)
	if err != nil {
		roomStatusError(w, err)
		return
	}
	admins, err := s.rooms.Admins(r.Context(), roomID)
	if err != nil {
		roomStatusError(w, err)
		return
	}
	data, err := s.audienceData(r.Context(), banned, admins, banned)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "could not read bans")
		return
	}
	for _, item := range data {
		item["silence"] = true
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": data})
}

func (s server) audiences(w http.ResponseWriter, r *http.Request, _ int64) {
	roomID, ok := httpx.PathID(w, r, "room")
	if !ok {
		return
	}
	if _, err := s.rooms.Info(r.Context(), roomID); err != nil {
		roomStatusError(w, err)
		return
	}
	members, err := s.rooms.Members(r.Context(), roomID)
	if err != nil {
		roomStatusError(w, err)
		return
	}
	admins, err := s.rooms.Admins(r.Context(), roomID)
	if err != nil {
		roomStatusError(w, err)
		return
	}
	muted, err := s.rooms.MutedUsers(r.Context(), roomID)
	if err != nil {
		roomStatusError(w, err)
		return
	}
	data, err := s.audienceData(r.Context(), members, admins, muted)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "could not read audiences")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": data})
}

func (s server) audiencesPage(w http.ResponseWriter, r *http.Request, _ int64) {
	roomID, ok := httpx.PathID(w, r, "room")
	if !ok {
		return
	}
	var input struct {
		Page     *int64 `json:"page"`
		PageSize *int64 `json:"pageSize"`
	}
	if !httpx.Decode(w, r, &input) {
		return
	}
	if _, err := s.rooms.Info(r.Context(), roomID); err != nil {
		roomStatusError(w, err)
		return
	}
	page, pageSize := int64(0), int64(10)
	if input.Page != nil && *input.Page > 0 {
		page = *input.Page
	}
	if input.PageSize != nil && *input.PageSize > 0 {
		pageSize = *input.PageSize
	}
	members, err := s.rooms.Members(r.Context(), roomID)
	if err != nil {
		roomStatusError(w, err)
		return
	}
	admins, err := s.rooms.Admins(r.Context(), roomID)
	if err != nil {
		roomStatusError(w, err)
		return
	}
	muted, err := s.rooms.MutedUsers(r.Context(), roomID)
	if err != nil {
		roomStatusError(w, err)
		return
	}
	pages := int64(0)
	if len(members) > 0 {
		pages = (int64(len(members)) + pageSize - 1) / pageSize
	}
	start := page * pageSize
	if start > int64(len(members)) {
		start = int64(len(members))
	}
	end := min(start+pageSize, int64(len(members)))
	data, err := s.audienceData(r.Context(), members[start:end], admins, muted)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "could not read audiences")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"status": "200", "data": data, "page": page, "pages": pages, "members": len(members),
	})
}

func (s server) audienceData(ctx context.Context, ids, admins, muted []int64) ([]map[string]any, error) {
	names, err := s.accounts.Names(ctx, ids)
	if err != nil {
		return nil, err
	}
	adminSet := make(map[int64]bool, len(admins))
	for _, id := range admins {
		adminSet[id] = true
	}
	mutedSet := make(map[int64]bool, len(muted))
	for _, id := range muted {
		mutedSet[id] = true
	}
	data := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		name := names[id]
		data = append(data, map[string]any{
			"id": strconv.FormatInt(id, 10), "searchName": name, "displayName": name,
			"avatar": "", "level": 0, "fanLevel": 0, "role": 2,
			"silence": mutedSet[id], "isAdmin": adminSet[id], "country": "",
			"spend": 0, "isExpired": 0, "planName": "", "teacherLabel": "",
			"toyInfo": "{}", "tokenId": nil, "equippedAdorn": map[string]any{},
		})
	}
	return data, nil
}

func twoPathIDs(w http.ResponseWriter, r *http.Request) (int64, int64, bool) {
	roomID, ok := httpx.PathID(w, r, "room")
	if !ok {
		return 0, 0, false
	}
	userID, ok := httpx.PathID(w, r, "uid")
	return roomID, userID, ok
}

func roomStatusError(w http.ResponseWriter, err error) {
	if errors.Is(err, live.ErrRoomNotFound) {
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "4005", "message": "data not found "})
		return
	}
	httpx.LiveError(w, err)
}
