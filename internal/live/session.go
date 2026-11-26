package live

import "time"

const DefaultAnchorNID uint32 = 200000001

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
