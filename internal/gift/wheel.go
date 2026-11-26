package gift

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"open_live_platform/internal/payment"
)

var ErrWheelClosed = errors.New("wheel is unavailable")

type WheelConfig struct {
	AnchorID   int64    `json:"anchor_id,string"`
	Price      int64    `json:"price"`
	Options    []string `json:"wheel_value"`
	OpenStatus int16    `json:"open_status"`
}

type WheelChange struct {
	Price   *int64
	Options *[]string
	Status  *int16
}

type WheelInput struct {
	RoomID, LiveID, UserID, AnchorID int64
}

type WheelOrder struct {
	WheelInput
	ID, Price      int64
	Key, PaymentNo string
	State          int
	Result         WheelResult
	Receipt        payment.Receipt
}

type WheelResult struct {
	Time          time.Time `json:"result_time"`
	PlatformLevel int32     `json:"platform_level"`
	FansLevel     int32     `json:"fans_level"`
	UserRole      int8      `json:"user_role"`
	UserID        int64     `json:"user_id,string"`
	UserAvatar    string    `json:"user_avatar"`
	UserName      string    `json:"user_name"`
	Value         string    `json:"result_value"`
}

type WheelStore interface {
	WheelConfig(context.Context, int64) (WheelConfig, error)
	UpdateWheelConfig(context.Context, int64, WheelChange) error
	ReserveWheel(context.Context, WheelOrder) (WheelOrder, bool, error)
	FinishWheel(context.Context, WheelOrder, payment.Receipt, bool) (WheelOrder, error)
	WheelHistory(context.Context, int64, int64, int64, int64) ([]WheelResult, int64, error)
	WheelRecommendations(context.Context) (map[string][]string, error)
}

func (o WheelOrder) Frames() ([][]byte, error) {
	room := strconv.FormatInt(o.RoomID, 10)
	frames := make([][]byte, 0, 3)
	for i, values := range []map[string]any{
		{"user_name": o.Result.UserName, "price": o.Price},
		{"wheel_value_id": o.ID, "wheel_value_value": o.Result.Value, "date_room_id": room},
		{"wheel_result": o.Result, "date_room_id": room},
	} {
		data, err := json.Marshal(map[string]any{
			"id": strconv.FormatInt(o.ID*3+int64(i), 10), "channel": "realtime", "room": room, "priority": 0,
			"sender": strconv.FormatInt(o.UserID, 10), "timestamp": strconv.FormatInt(o.Result.Time.Unix(), 10),
			"message": map[string]any{"msgGroup": "gift", "msgType": 12 + i, "message": values},
		})
		if err != nil {
			return nil, err
		}
		frames = append(frames, data)
	}
	return frames, nil
}
