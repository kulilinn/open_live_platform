package live

import (
	"slices"
	"time"
)

const HistoryLimit = 100

const historyTTL = 24 * time.Hour

type ChatMessage struct {
	ID        int64     `json:"id"`
	RoomID    int64     `json:"room_id"`
	UserID    int64     `json:"user_id"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
	Payload   string    `json:"-"`
}

// HistoryPage returns a copy of a zero-based page ordered newest first.
// Messages must be ordered by descending ID. A zero size returns all retained messages.
func HistoryPage(messages []ChatMessage, page, size uint64, now time.Time) []ChatMessage {
	if len(messages) == 0 || now.Sub(messages[0].CreatedAt) >= historyTTL {
		return []ChatMessage{}
	}
	messages = messages[:min(len(messages), HistoryLimit)]
	// Every message renews the window. A gap of at least 24 hours starts a fresh history.
	for i := 1; i < len(messages); i++ {
		if messages[i-1].CreatedAt.Sub(messages[i].CreatedAt) >= historyTTL {
			messages = messages[:i]
			break
		}
	}
	if size == 0 {
		return slices.Clone(messages)
	}
	count := uint64(len(messages))
	if page > (count-1)/size {
		return []ChatMessage{}
	}
	start := page * size
	return slices.Clone(messages[start : start+min(size, count-start)])
}
