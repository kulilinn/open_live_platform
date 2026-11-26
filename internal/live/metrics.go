package live

import (
	"context"
	"errors"
	"math"
	"time"

	"open_live_platform/internal/gift"
)

var ErrLikeCount = errors.New("invalid like count")

type RoomSettlement struct {
	Time          int64  `json:"time"`
	Income        int64  `json:"income"`
	FanCount      int64  `json:"fanCount"`
	NewFanCount   int64  `json:"newFanCount"`
	GiftUserCount int64  `json:"giftUserCount"`
	Banner        string `json:"banner"`
}

type RoomReport struct {
	Room        string `json:"-"`
	ReporterID  int64  `json:"-"`
	Subject     string `json:"subject"`
	Description string `json:"description"`
}

type RoomMetricsStore interface {
	AddRoomLikes(ctx context.Context, roomID, liveID, userID, count int64) (int64, error)
	RoomSettlement(ctx context.Context, liveID, ownerID int64) (RoomSettlement, error)
	ReportRoom(ctx context.Context, report RoomReport) error
}

type roomMetrics struct {
	roomID, ownerID int64
	started, ended  time.Time
	maxAudience     int64
	likes           int64
}

func (s *Store) AddRoomLikes(ctx context.Context, roomID, liveID, userID, count int64) (int64, error) {
	if count <= 0 {
		return 0, ErrLikeCount
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	room, err := s.commandRoom(ctx, roomID)
	if err != nil {
		return 0, err
	}
	if room.Session.LiveID != liveID {
		return 0, ErrRoomClosed
	}
	metric := s.metrics[liveID]
	if count > math.MaxInt64-metric.likes {
		return 0, ErrLikeCount
	}
	metric.likes += count
	room.publish(RoomLikeEvent(roomID, liveID, userID, metric.likes, time.Now().UTC()))
	return metric.likes, nil
}

func RoomLikeEvent(roomID, liveID, userID, likes int64, at time.Time) Event {
	return Event{Type: "room_like", RoomID: roomID, LiveID: liveID, UserID: userID, Likes: likes, CreatedAt: at}
}

func (s *Store) RoomSettlement(ctx context.Context, liveID, ownerID int64) (RoomSettlement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return RoomSettlement{}, err
	}
	metric := s.metrics[liveID]
	if metric == nil {
		return RoomSettlement{}, ErrRoomNotFound
	}
	if metric.ownerID != ownerID {
		return RoomSettlement{}, ErrForbidden
	}
	end := metric.ended
	if end.IsZero() {
		end = time.Now()
	}
	data := RoomSettlement{Time: end.Unix() - metric.started.Unix(), FanCount: metric.maxAudience, Banner: s.rooms[metric.roomID].Banner}
	senders := make(map[int64]struct{})
	for _, order := range s.giftOrders {
		if order.Input.LiveID == liveID && order.State == gift.Paid {
			data.Income += order.Income
			senders[order.Input.SenderID] = struct{}{}
		}
	}
	data.GiftUserCount = int64(len(senders))
	return data, nil
}

func (s *Store) ReportRoom(ctx context.Context, report RoomReport) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	s.reports = append(s.reports, report)
	return nil
}
