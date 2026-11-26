package httpapi

import (
	"context"
	"net/http"
	"strconv"

	"open_live_platform/internal/live"
	"open_live_platform/internal/transport/httpx"
)

func (s server) roomInfo(ctx context.Context, userID, roomID int64) (map[string]any, error) {
	room, err := s.rooms.Info(ctx, roomID)
	if err != nil {
		return nil, err
	}
	members, err := s.rooms.Members(ctx, roomID)
	if err != nil {
		return nil, err
	}
	names, err := s.accounts.Names(ctx, []int64{room.OwnerID})
	if err != nil {
		return nil, err
	}
	memberIDs := make([]string, 0, len(members))
	for _, id := range members {
		memberIDs = append(memberIDs, strconv.FormatInt(id, 10))
	}
	online := 0
	if room.Online {
		online = 1
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
		"fansCount": 0, "anchor": roomIDText, "anchorNid": 0,
		"banner": "", "subject": room.Title, "title": room.Title,
		"tag": "", "description": "", "online": online, "activity": 0,
		"members": memberIDs, "memberCount": len(members), "member_count": len(members),
		"isAnchor": userID == room.OwnerID, "showing": false,
		"chatId": roomIDText, "liveId": roomIDText, "gifts": []any{}, "plan": []any{},
		"offlineInfo": "", "limited": false, "limitedTime": nil, "fans": []any{},
		"area": "", "toyLabel": false, "hotLabel": "", "systemMessages": []any{},
		"teacherLabel": "", "ticketLabel": "", "role": role, "silence": false,
		"subLevel": nil, "freePlanId": "0", "toyInfo": "{}",
		"trans": false, "transLang": "", "promptOpen": nil,
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
	info, err := s.roomInfo(r.Context(), userID, roomID)
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
