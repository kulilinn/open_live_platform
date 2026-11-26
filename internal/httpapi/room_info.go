package httpapi

import (
	"context"
	"net/http"
	"strconv"

	"open_live_platform/internal/live"
	"open_live_platform/internal/transport/httpx"
)

func (s server) roomInfo(ctx context.Context, userID int64, room live.Room) (map[string]any, error) {
	members, err := s.rooms.Members(ctx, room.ID)
	if err != nil {
		return nil, err
	}
	return s.roomInfoWithMembers(ctx, userID, room, members)
}

func (s server) roomInfoWithMembers(ctx context.Context, userID int64, room live.Room, members []int64) (map[string]any, error) {
	names, err := s.accounts.Names(ctx, []int64{room.OwnerID})
	if err != nil {
		return nil, err
	}
	memberIDs := make([]string, 0, len(members))
	for _, id := range members {
		memberIDs = append(memberIDs, strconv.FormatInt(id, 10))
	}
	role := 2
	if userID == room.OwnerID {
		role = 3
	}
	ownerName := names[room.OwnerID]
	roomIDText := strconv.FormatInt(room.ID, 10)
	return map[string]any{
		"id": room.ID, "owner_id": room.OwnerID,
		"room": roomIDText, "name": ownerName, "avatar": "",
		"fansCount": 0, "anchor": strconv.FormatInt(room.OwnerID, 10), "anchorNid": room.Session.AnchorNID,
		"banner": "", "subject": room.Title, "title": room.Title,
		"tag": "", "description": "", "online": room.Status(), "activity": 0,
		"members": memberIDs, "memberCount": room.MemberCount, "member_count": room.MemberCount,
		"isAnchor": userID == room.OwnerID, "showing": false,
		"chatId": roomIDText, "liveId": strconv.FormatInt(room.Session.LiveID, 10), "gifts": []any{}, "plan": []any{},
		"offlineInfo": "", "limited": false, "limitedTime": nil, "fans": []any{},
		"area": "", "toyLabel": false, "hotLabel": "", "systemMessages": []any{},
		"teacherLabel": "", "ticketLabel": "", "role": role, "silence": false,
		"subLevel": nil, "freePlanId": "0", "toyInfo": "{}",
		"trans": false, "transLang": room.TranslationLanguage, "promptOpen": nil,
		"searchName": ownerName, "isLock": room.IsLock,
	}, nil
}

func (s server) infoRoom(w http.ResponseWriter, r *http.Request, userID int64) {
	roomID, ok := httpx.PathID(w, r, "room")
	if !ok {
		return
	}
	room, err := s.rooms.Info(r.Context(), roomID)
	if err != nil {
		httpx.LiveError(w, err)
		return
	}
	info, err := s.roomInfo(r.Context(), userID, room)
	if err != nil {
		httpx.LiveError(w, err)
		return
	}
	response := legacyRoomJSON(room)
	response["status"] = "200"
	response["data"] = info
	httpx.JSON(w, http.StatusOK, response)
}

func legacyRoomJSON(room live.Room) map[string]any {
	return map[string]any{
		"id": room.ID, "owner_id": room.OwnerID, "title": room.Title,
		"online": room.Online, "member_count": room.MemberCount, "isLock": room.IsLock,
	}
}
