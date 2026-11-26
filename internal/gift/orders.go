package gift

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"time"

	"open_live_platform/internal/payment"
)

var (
	ErrOrderNotFound = errors.New("gift order not found")
	ErrOrderConflict = errors.New("gift idempotency key already used for another purchase")
	ErrPending       = errors.New("gift payment requires reconciliation")
	ErrUnsupported   = errors.New("gift pricing or effects unavailable")
	ErrLimit         = errors.New("gift purchase limit")
)

const (
	Pending = iota
	Paid
	Declined
)

type Input struct {
	Room       string `json:"date_room_id"`
	LiveID     int64  `json:"live_id,string"`
	GiftID     int64  `json:"gift_id,string"`
	LimitID    int64  `json:"limit_id,string"`
	SenderID   int64  `json:"sender_id,string"`
	ReceiverID int64  `json:"receiver_id,string"`
	Quantity   int32  `json:"quantity"`
}

type User struct {
	ID            int64  `json:"id,string"`
	Name          string `json:"name"`
	Photo         string `json:"photo"`
	SearchName    string `json:"searchName"`
	PlatformLevel int32  `json:"platformLevel"`
	FanLevel      int32  `json:"fanLevel"`
}

type Order struct {
	ID                int64
	Input             Input
	Key, PaymentOrder string
	Item              Gift
	Sender, Receiver  User
	SenderRole        int8
	Cost, Income      int64
	State             int
	PaidAt            time.Time
	Receipt           payment.Receipt
	Result            Result
}

type Result struct {
	Quota       bool            `json:"quota_flag"`
	Success     json.RawMessage `json:"success_info"`
	Performance json.RawMessage `json:"performance_info"`
}

type Rank struct{ UserID, Income int64 }

type Income struct {
	GivenAt    time.Time `json:"give_time"`
	GiftID     int64     `json:"gift_id,string"`
	GiftName   string    `json:"gift_name"`
	GiftIcon   string    `json:"gift_icon"`
	Points     int64     `json:"income"`
	SenderName string    `json:"sender_name"`
	SenderID   int64     `json:"sender_id,string"`
}

type OrderStore interface {
	FindGiftOrder(ctx context.Context, senderID int64, key string) (Order, error)
	ReserveGift(ctx context.Context, roomID int64, candidate Order) (order Order, created bool, err error)
	FinishGift(ctx context.Context, roomID int64, order Order, receipt payment.Receipt, declined bool) (Order, error)
	GiftIncome(ctx context.Context, roomID, liveID int64) ([]Income, error)
	GiftTotalIncome(ctx context.Context, roomID int64) (*int64, error)
	GiftRank(ctx context.Context, anchorID int64, since time.Time) ([]Rank, error)
}

type LimitStore interface {
	GiftLimitQuote(ctx context.Context, input Input, item Gift) (int64, int64, error)
	GiftPromotionContent(ctx context.Context, userID int64, content Interactions) (Interactions, error)
}

func (l Limit) Cost(item Gift, quantity int32) (int64, int64, error) {
	if l.GiftID != item.ID || l.Income == nil {
		return 0, 0, ErrUnsupported
	}
	if _, _, err := item.Cost(quantity, 0); err != nil {
		return 0, 0, err
	}
	if l.Price < 0 || l.Price > math.MaxInt64/int64(quantity) {
		return 0, 0, ErrUnsupported
	}
	cost, income := l.Price*int64(quantity), *l.Income
	if item.EffectControlType == RefuelingEffect {
		income = 0
	}
	if income < 0 || income > cost {
		return 0, 0, ErrUnsupported
	}
	return cost, income, nil
}

func (g Gift) Cost(quantity int32, limitID int64) (int64, int64, error) {
	if limitID != 0 || (g.EffectControlType != 0 && !g.QueuesEffect() && g.EffectControlType != RefuelingEffect) ||
		quantity <= 0 || g.Price < 0 || g.Price > math.MaxInt64/int64(quantity) {
		return 0, 0, ErrUnsupported
	}
	cost, income := g.Price*int64(quantity), int64(g.IncomeStream)
	if g.EffectControlType == RefuelingEffect {
		income = 0 // The awarded gifts each have their own settlement.
	}
	if income < 0 || income > cost {
		return 0, 0, ErrUnsupported
	}
	return cost, income, nil
}

func Yen(points int64) (int64, error) {
	whole, remainder := points/50, points%50*789/50
	if points < 0 || whole > (math.MaxInt64-remainder)/789 {
		return 0, ErrUnsupported
	}
	return whole*789 + remainder, nil
}

func (o Order) IncomeLine() Income {
	return Income{GivenAt: o.PaidAt.UTC(), GiftID: o.Input.GiftID, GiftName: o.Item.Name,
		GiftIcon: o.Item.Icon, Points: o.Income, SenderName: o.Sender.Name, SenderID: o.Input.SenderID}
}

func (o Order) BuildResult() (Result, error) {
	yen, err := Yen(o.Income)
	if err != nil {
		return Result{}, err
	}
	item := o.Item
	var duration int64
	if item.ParameterOne != nil {
		duration, _ = strconv.ParseInt(*item.ParameterOne, 10, 64)
	}
	timestamp := o.PaidAt.Unix()
	successType := 1
	if item.EffectControlType == RocketEffect {
		successType = 4
	}
	if item.EffectControlType == RefuelingEffect {
		return Result{}, nil
	}
	success, err := o.payload(successType, o.ID*2, timestamp, map[string]any{
		"gift_id": item.ID, "gift_name": item.Name, "gift_icon": item.Icon, "gift_type": item.GiftType,
		"vibration_intensity": item.VibrationIntensity, "vibration_duration": duration, "effect_control_type": item.EffectControlType,
		"sender_role": o.SenderRole, "sender_info": o.Sender, "receiver_info": o.Receiver,
		"combo_count": o.Input.Quantity, "yen": yen, "is_expire": nil, "timestamp": o.PaidAt.UnixMilli(), "anchorIncome": o.Income,
	})
	result := Result{Success: success}
	if err != nil || item.PerformanceTime <= 0 || item.PerformanceRule < 1 || item.PerformanceRule > 3 {
		return result, err
	}
	msgType := 3
	if item.PerformanceRule == 1 {
		msgType = 2
	}
	var effects string
	if item.SpecialEffects != nil {
		effects = *item.SpecialEffects
	}
	result.Performance, err = o.payload(msgType, o.ID*2+1, timestamp, map[string]any{
		"current_expired_gift": "", "next_start_gift": map[string]any{
			"gift_id": strconv.FormatInt(item.ID, 10), "gift_name": item.Name, "gift_icon": item.Icon, "gift_price": item.Price,
			"special_effects": effects, "floating_effects": "", "is_entry": false,
			"performance_rule": item.PerformanceRule, "operation_time": item.PerformanceTime, "floating_time": 0,
			"sender_info": o.Sender, "receiver_info": o.Receiver, "give_time": timestamp, "send_time": timestamp,
			"combo_count": o.Input.Quantity, "quantity": o.Input.Quantity, "queue_v_id": o.ID,
		},
	})
	return result, err
}

func (o Order) payload(messageType int, id, timestamp int64, message any) (json.RawMessage, error) {
	return json.Marshal(map[string]any{
		"id": strconv.FormatInt(id, 10), "channel": "realtime", "room": o.Input.Room, "priority": 0,
		"sender": strconv.FormatInt(o.Input.SenderID, 10), "timestamp": strconv.FormatInt(timestamp, 10),
		"message": map[string]any{"msgGroup": "gift", "msgType": messageType, "message": message},
	})
}
