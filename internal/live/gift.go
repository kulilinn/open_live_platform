package live

import (
	"cmp"
	"context"
	"math"
	"slices"
	"time"

	"open_live_platform/internal/gift"
	"open_live_platform/internal/payment"
)

var _ gift.OrderStore = (*Store)(nil)

type giftOrderKey struct {
	sender int64
	key    string
}

func (s *Store) FindGiftOrder(ctx context.Context, senderID int64, key string) (gift.Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return gift.Order{}, err
	}
	order, exists := s.giftOrders[giftOrderKey{senderID, key}]
	if !exists {
		return gift.Order{}, gift.ErrOrderNotFound
	}
	return order, nil
}

func (s *Store) ReserveGift(ctx context.Context, roomID int64, candidate gift.Order) (gift.Order, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return gift.Order{}, false, err
	}
	key := giftOrderKey{candidate.Input.SenderID, candidate.Key}
	if order, exists := s.giftOrders[key]; exists {
		if order.Input != candidate.Input {
			return gift.Order{}, false, gift.ErrOrderConflict
		}
		return order, false, nil
	}
	room, err := s.onlineRoom(roomID)
	if err != nil {
		return gift.Order{}, false, err
	}
	if candidate.Input.LiveID != room.Session.LiveID {
		return gift.Order{}, false, ErrRoomClosed
	}
	if !room.hasMember(candidate.Input.SenderID) || candidate.Input.ReceiverID != room.OwnerID || candidate.Input.SenderID == room.OwnerID {
		return gift.Order{}, false, ErrForbidden
	}
	if _, admin := room.admins[candidate.Input.SenderID]; admin {
		candidate.SenderRole = 4
	}
	if s.giftOrders == nil {
		s.giftOrders = make(map[giftOrderKey]gift.Order)
		s.nextGiftOrderID = 1 << 34
	}
	candidate.ID = s.nextGiftOrderID
	s.nextGiftOrderID++
	s.giftOrders[key] = candidate
	return candidate, true, nil
}

func (s *Store) FinishGift(ctx context.Context, roomID int64, candidate gift.Order, receipt payment.Receipt, declined bool) (gift.Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return gift.Order{}, err
	}
	key := giftOrderKey{candidate.Input.SenderID, candidate.Key}
	order, exists := s.giftOrders[key]
	if !exists || order.ID != candidate.ID {
		return gift.Order{}, gift.ErrOrderNotFound
	}
	if order.State != gift.Pending {
		return order, nil
	}
	if declined {
		order.State = gift.Declined
		s.giftOrders[key] = order
		return order, nil
	}
	if order.Cost > 0 && (receipt.OrderID <= 0 || receipt.TotalPoints != order.Cost) {
		return gift.Order{}, gift.ErrPending
	}
	if order.Cost > 0 {
		if s.giftReceipts == nil {
			s.giftReceipts = make(map[int64]struct{})
		}
		if _, exists := s.giftReceipts[receipt.OrderID]; exists {
			return gift.Order{}, gift.ErrPending
		}
	}
	order.State, order.Receipt, order.PaidAt = gift.Paid, receipt, time.Now().UTC()
	var err error
	order.Result, err = order.BuildResult()
	if err != nil {
		return gift.Order{}, err
	}
	if order.Cost > 0 {
		s.giftReceipts[receipt.OrderID] = struct{}{}
	}
	s.giftOrders[key] = order
	if room := s.rooms[roomID]; room != nil && room.Online && room.Session.LiveID == order.Input.LiveID {
		room.publish(Event{Type: "gift", UserID: order.Input.SenderID, Payload: string(order.Result.Success), CreatedAt: order.PaidAt})
		if len(order.Result.Performance) != 0 {
			room.publish(Event{Type: "gift", UserID: order.Input.SenderID, Payload: string(order.Result.Performance), CreatedAt: order.PaidAt})
		}
	}
	return order, nil
}

func (s *Store) GiftIncome(ctx context.Context, roomID, liveID int64) ([]gift.Income, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	orders := []gift.Order{}
	for _, order := range s.giftOrders {
		if order.State == gift.Paid && order.Input.LiveID == liveID && order.Input.ReceiverID == roomID {
			orders = append(orders, order)
		}
	}
	slices.SortFunc(orders, func(a, b gift.Order) int {
		if order := b.PaidAt.Compare(a.PaidAt); order != 0 {
			return order
		}
		return cmp.Compare(b.ID, a.ID)
	})
	result := make([]gift.Income, 0, len(orders))
	for _, item := range orders {
		result = append(result, item.IncomeLine())
	}
	return result, nil
}

func (s *Store) GiftRank(ctx context.Context, anchorID int64, since time.Time) ([]gift.Rank, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	totals := make(map[int64]int64)
	for _, order := range s.giftOrders {
		if order.State == gift.Paid && order.Input.ReceiverID == anchorID && order.Income > 0 && order.PaidAt.After(since) {
			sender := order.Input.SenderID
			if totals[sender] > math.MaxInt64-order.Income {
				return nil, gift.ErrUnsupported
			}
			totals[sender] += order.Income
		}
	}
	result := make([]gift.Rank, 0, len(totals))
	for userID, total := range totals {
		result = append(result, gift.Rank{UserID: userID, Income: total})
	}
	slices.SortFunc(result, func(a, b gift.Rank) int {
		if order := cmp.Compare(b.Income, a.Income); order != 0 {
			return order
		}
		return cmp.Compare(a.UserID, b.UserID)
	})
	return result[:min(100, len(result))], nil
}

func (s *Store) GiftTotalIncome(ctx context.Context, roomID int64) (*int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var total *int64
	for _, order := range s.giftOrders {
		if order.State != gift.Paid || order.Input.ReceiverID != roomID {
			continue
		}
		if total == nil {
			total = new(int64)
		}
		if *total > math.MaxInt64-order.Income {
			return nil, gift.ErrUnsupported
		}
		*total += order.Income
	}
	return total, nil
}
