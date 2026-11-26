package date

import (
	"context"
	"errors"
	"math"
	"strconv"
	"time"

	"open_live_platform/internal/gift"
	"open_live_platform/internal/payment"
)

var ErrGameWaiting = errors.New("previous game gift is awaiting acceptance")

const (
	GiftPending = iota
	GiftPaid
	GiftDeclined
	GiftWaiting
	GiftRefused
)

type GiftInput struct {
	Channel  int64 `json:"dateRoomId,string"`
	GiftID   int64 `json:"giftId,string"`
	Sender   int64 `json:"senderId,string"`
	Receiver int64 `json:"receiverId,string"`
	Quantity int32 `json:"quantity"`
}

type GiftOrder struct {
	ID, Cost, Income int64
	Input            GiftInput
	State            int
	Key, PaymentNo   string
	Item             gift.Gift
	Sender, Receiver gift.User
	PaidAt           time.Time
	Receipt          payment.Receipt
}

func (o GiftOrder) Duration() int64 {
	if o.Item.ParameterOne != nil {
		seconds, _ := strconv.ParseInt(*o.Item.ParameterOne, 10, 64)
		return seconds
	}
	return 0
}

type GiftQueueEntry struct {
	ID    int64
	Start time.Time
	End   time.Time
	Order GiftOrder
}

type GiftStore interface {
	FindDateGift(context.Context, int64, string) (GiftOrder, error)
	ReserveDateGift(context.Context, GiftOrder) (GiftOrder, bool, error)
	DecideDateGift(ctx context.Context, channel, orderID, actor int64, accept bool) (GiftOrder, bool, error)
	FinishDateGift(context.Context, GiftOrder, payment.Receipt, bool) (GiftOrder, bool, error)
	DateGifts(ctx context.Context, channel int64, state int) ([]GiftOrder, error)
	DateGiftQueue(context.Context, int64) ([]GiftQueueEntry, error)
	ClaimDateGiftStarts(context.Context) ([]GiftQueueEntry, error)
}

func GiftCost(input GiftInput, item gift.Gift) (int64, error) {
	if input.Quantity <= 0 || input.Quantity > 100 || item.Price < 0 || item.Price > math.MaxInt64/int64(input.Quantity) {
		return 0, gift.ErrUnsupported
	}
	if item.GiftType == 2 {
		duration := (GiftOrder{Item: item}).Duration()
		if item.EffectControlType < 2 || item.EffectControlType > 5 || duration <= 0 || duration > 3600 {
			return 0, gift.ErrUnsupported
		}
	}
	return item.Price * int64(input.Quantity), nil
}

func GiftView(o GiftOrder, id int64, seconds int64, end time.Time) map[string]any {
	g := o.Item
	var arg string
	var strength int32
	if g.ParameterOne != nil {
		arg = *g.ParameterOne
	}
	if g.ParameterTwo != nil {
		strength = *g.ParameterTwo
	}
	var effect any = "Unknown"
	switch g.EffectControlType {
	case 0:
		effect = map[string]any{"Ordinary": arg}
	case 1:
		effect = map[string]any{"Game": arg}
	case 2:
		effect = map[string]any{"Toy": map[string]any{"ControlAnchor": []any{arg, strength}}}
	case 3:
		effect = map[string]any{"Toy": map[string]string{"FullControl": arg}}
	case 4:
		effect = map[string]any{"Toy": map[string]string{"AnchorControl": arg}}
	case 5:
		effect = map[string]any{"Toy": map[string]string{"TwoWayControl": arg}}
	}
	var until int64
	if !end.IsZero() {
		until = end.Unix()
	}
	return map[string]any{
		"id": strconv.FormatInt(id, 10), "name": g.Name, "price": g.Price, "icon": g.Icon,
		"giftType": g.GiftType, "specialEffects": g.SpecialEffects, "animation": g.Animation,
		"normalEffects": g.NormalEffects, "isCustom": g.IsCustom, "giftSort": g.GiftSort,
		"recommendedDescription": g.RecommendedDescription, "effectControlType": g.EffectControlType,
		"vibrationIntensity": g.VibrationIntensity, "toyInspection": g.ToyInspection, "giftTypeEnum": effect,
		"operationTime": seconds, "endTime": until,
	}
}
