package live

import "time"

const DefaultAnchorNID uint32 = 200000001

type RoomStatus uint8

const (
	RoomOffline    RoomStatus = 0
	RoomOnline     RoomStatus = 1
	RoomAnchorAway RoomStatus = 3
)

// Status reports the Rust room status. Online describes whether a broadcast is active.
func (r Room) Status() RoomStatus {
	if !r.Online {
		return RoomOffline
	}
	if r.AnchorAway {
		return RoomAnchorAway
	}
	return RoomOnline
}

// LiveSession identifies a broadcast independently of its room.
type LiveSession struct {
	LiveID    int64
	AnchorNID uint32
	StartedAt time.Time
	EndedAt   time.Time
}

func (s LiveSession) Duration(now time.Time) time.Duration {
	if s.StartedAt.IsZero() {
		return 0
	}
	if !s.EndedAt.IsZero() {
		now = s.EndedAt
	}
	return max(0, now.Sub(s.StartedAt))
}
