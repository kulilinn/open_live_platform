package relay

import (
	"context"
	"strconv"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/live"
)

func h5RoomLike(ctx context.Context, accounts auth.AccountStore, room string, event live.Event) any {
	sender := strconv.FormatInt(event.UserID, 10)
	actor, _, _ := accounts.Find(ctx, event.UserID)
	profile := auth.PublicProfile(actor)
	return h5RoomFrame(room, map[string]any{
		"id": strconv.FormatInt(event.CreatedAt.UnixNano(), 10), "channel": "room_event", "room": room,
		"priority": 0, "sender": sender, "timestamp": strconv.FormatInt(event.CreatedAt.Unix(), 10),
		"message": map[string]any{
			"msgType": "Z", "msgGroup": "chat",
			"message": map[string]any{
				"msgType": "Z", "msgGroup": "chat", "roomId": room, "liveId": strconv.FormatInt(event.LiveID, 10),
				"thumbPeople": sender, "curentLikeCount": event.Likes,
			},
			"userInfo": map[string]any{
				"uid": event.UserID, "role": 5, "name": actor.Account, "avatar": profile.Photo,
				"searchName": profile.SearchName, "country": "", "userLevel": 0, "fanLevel": 0,
				"isGuardExpired": false, "planName": nil,
			},
		},
	})
}
