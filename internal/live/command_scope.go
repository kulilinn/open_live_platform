package live

import "context"

type roomCommandKey struct{}

type roomCommandScope struct {
	roomID int64
	liveID int64
}

// WithRoomCommandScope binds a relay command to its server-authorized subscription.
func WithRoomCommandScope(ctx context.Context, roomID, liveID int64) context.Context {
	return context.WithValue(ctx, roomCommandKey{}, roomCommandScope{roomID: roomID, liveID: liveID})
}

// RoomCommandScope returns the expected room and broadcast for a scoped command.
func RoomCommandScope(ctx context.Context) (roomID, liveID int64, scoped bool) {
	scope, ok := ctx.Value(roomCommandKey{}).(roomCommandScope)
	return scope.roomID, scope.liveID, ok
}
