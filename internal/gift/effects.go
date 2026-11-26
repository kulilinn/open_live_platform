package gift

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"
)

const RocketEffect int8 = 6

var ErrEffectBusy = errors.New("gift effect is already active")

func (g Gift) EffectSeconds() int64 {
	if g.ParameterOne == nil {
		return 0
	}
	duration, _ := strconv.ParseInt(*g.ParameterOne, 10, 64)
	return duration
}

func (g Gift) QueuesEffect() bool {
	duration := g.EffectSeconds()
	return duration > 0 && duration <= 3600 &&
		(g.EffectControlType == RocketEffect || (g.GiftType == 2 && g.EffectControlType >= 2 && g.EffectControlType <= 5))
}

type RoomEffectsStore interface {
	ReduceGiftDuration(context.Context, int64, int64, float64) error
	GiftDurationReduction(context.Context, int64) (float64, error)
	RoomHardware(context.Context, int64, int64) ([]RoomEffect, error)
	RoomRocket(context.Context, int64, int64) (*RoomEffect, error)
	EndRoomEffect(context.Context, int64, int64, bool, int64) error
	IncreaseRocketSpeed(context.Context, int64, int64, int64, int32, float64) error
	RocketRank(context.Context, int64) ([]RocketContribution, error)
	RocketWasLaunched(context.Context, int64, int64) (bool, error)
}

type RoomEffect struct {
	Order       Order
	Start, End  time.Time
	Ended       time.Time
	Speed       int32
	FinalResult []byte
}

type RocketContribution struct {
	User  int64  `json:"user_id,string"`
	Name  string `json:"user_name"`
	Photo string `json:"user_photo"`
	Score int64  `json:"scores,string"`
}

func (e RoomEffect) Active(now time.Time) bool {
	return e.Ended.IsZero() && now.Before(e.End)
}

func (e RoomEffect) HardwareView() map[string]any {
	item := e.Order.Item
	var p2 int32
	var p3 int64
	if item.ParameterTwo != nil {
		p2 = *item.ParameterTwo
	}
	if item.ParameterThree != nil {
		p3 = *item.ParameterThree
	}
	return map[string]any{
		"gift_id": strconv.FormatInt(item.ID, 10), "limit_id": strconv.FormatInt(e.Order.Input.LimitID, 10),
		"gift_name": item.Name, "gift_icon": item.Icon, "effect_control_type": item.EffectControlType,
		"operation_time": int64(e.End.Sub(e.Start) / time.Second), "parameter_two": p2, "parameter_three": p3,
		"vibration_intensity": item.VibrationIntensity, "waveform_id": p2, "sender_info": e.Order.Sender,
		"receiver_info": e.Order.Receiver, "give_time": e.Order.PaidAt.Unix(), "send_time": e.Start.Unix(),
		"hardware_bottom_frame": nil, "device": nil, "end_time": strconv.FormatInt(e.End.Unix(), 10),
	}
}

func (e RoomEffect) HardwareFrame() ([]byte, error) {
	return json.Marshal(map[string]any{
		"id": strconv.FormatInt(e.Order.ID*2+1, 10), "channel": "realtime", "room": e.Order.Input.Room, "priority": 0,
		"sender": strconv.FormatInt(e.Order.Input.SenderID, 10), "timestamp": strconv.FormatInt(e.Start.Unix(), 10),
		"message": map[string]any{"msgGroup": "gift", "msgType": 8, "message": map[string]any{
			"current_expired_gift": "", "next_start_gift": e.HardwareView(),
		}},
	})
}
