package httpapi

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"time"

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
	role := 0
	if userID == room.OwnerID {
		role = 3
	} else {
		admins, err := s.rooms.Admins(ctx, room.ID)
		if err != nil {
			return nil, err
		}
		switch {
		case slices.Contains(admins, userID):
			role = 4
		case slices.Contains(members, userID):
			role = 2
		}
	}
	muted, err := s.rooms.Muted(ctx, room.ID, userID)
	if err != nil {
		return nil, err
	}
	return roomInfoFields(userID, room, names[room.OwnerID], members, role, muted), nil
}

func roomInfoFields(userID int64, room live.Room, ownerName string, members []int64, role int, muted bool) map[string]any {
	memberIDs := make([]string, 0, len(members))
	for _, id := range members {
		memberIDs = append(memberIDs, strconv.FormatInt(id, 10))
	}
	roomIDText := strconv.FormatInt(room.ID, 10)
	limited := room.Limited(time.Now())
	var limitedTime any
	if limited {
		limitedTime = strconv.FormatInt(room.ModerationUntil, 10)
	}
	return map[string]any{
		"id": room.ID, "owner_id": room.OwnerID,
		"room": roomIDText, "name": ownerName, "avatar": "",
		"fansCount": 0, "anchor": strconv.FormatInt(room.OwnerID, 10), "anchorNid": live.DefaultAnchorNID,
		"banner": room.Banner, "subject": room.Title, "title": room.Title,
		"tag": room.Tag, "description": room.Description, "online": room.Status(), "activity": 0,
		"members": memberIDs, "memberCount": room.MemberCount, "member_count": room.MemberCount,
		"isAnchor": userID == room.OwnerID, "showing": false,
		"chatId": roomIDText, "liveId": strconv.FormatInt(room.Session.LiveID, 10), "gifts": room.Gifts, "plan": room.Plan,
		"offlineInfo": room.OfflineInfo, "limited": limited, "limitedTime": limitedTime, "fans": []any{},
		"area": "", "toyLabel": false, "hotLabel": "0", "systemMessages": []any{},
		"teacherLabel": "", "ticketLabel": "", "role": role, "silence": muted,
		"subLevel": -1, "freePlanId": "0", "toyInfo": "{}",
		"trans": room.TranslationEnabled, "transLang": room.TranslationLanguage, "promptOpen": nil,
		"searchName": ownerName, "isLock": room.IsLock,
	}
}

func (s server) infoRoom(w http.ResponseWriter, r *http.Request, userID int64) {
	var roomID int64
	rust := rustRoomCompat(r)
	if rust {
		var err error
		roomID, err = strconv.ParseInt(r.PathValue("room"), 10, 64)
		if err != nil || roomID < 1 {
			roomStatusError(w, live.ErrRoomNotFound)
			return
		}
	} else {
		var ok bool
		roomID, ok = httpx.PathID(w, r, "room")
		if !ok {
			return
		}
	}
	room, err := s.rooms.Info(r.Context(), roomID)
	if err != nil {
		if rust {
			roomStatusError(w, err)
			return
		}
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

func (s server) roomSettingsInfo(ctx context.Context, room live.Room) (map[string]any, error) {
	names, err := s.accounts.Names(ctx, []int64{room.OwnerID})
	if err != nil {
		return nil, err
	}
	info := roomInfoFields(0, room, names[room.OwnerID], nil, 0, false)
	info["anchorNid"] = 0
	info["memberCount"] = 0
	info["member_count"] = 0
	info["subLevel"] = nil
	info["searchName"] = ""
	info["toyLabel"] = true
	return info, nil
}

func legacyRoomJSON(room live.Room) map[string]any {
	return map[string]any{
		"id": room.ID, "owner_id": room.OwnerID, "title": room.Title,
		"online": room.Online, "member_count": room.MemberCount, "isLock": room.IsLock,
	}
}
