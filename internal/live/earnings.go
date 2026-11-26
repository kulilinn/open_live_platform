package live

import (
	"context"
	"errors"
	"math"
	"sort"

	"open_live_platform/internal/gift"
)

func (s *Store) earningOrders(ctx context.Context, f gift.EarningsFilter, includeStart bool) ([]gift.EarningsLine, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := make([]gift.EarningsLine, 0)
	if f.Type != nil && *f.Type != gift.StreamGiftIncomeType {
		return result, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, order := range s.giftOrders {
		if order.Input.ReceiverID == f.AnchorID && order.State == gift.Paid &&
			(order.PaidAt.After(f.From) || (includeStart && order.PaidAt.Equal(f.From))) && !order.PaidAt.After(f.To) {
			result = append(result, order.EarningsLine())
		}
	}
	return result, nil
}

func (s *Store) EarningsSummary(ctx context.Context, f gift.EarningsFilter) ([]gift.EarningsBucket, error) {
	lines, err := s.earningOrders(ctx, f, true)
	if err != nil {
		return nil, err
	}
	totals := make(map[string]int64)
	for _, row := range lines {
		date := f.Bucket(row.CreatedAt)
		if totals[date] > math.MaxInt64-row.Points {
			return nil, errors.New("earnings bucket exceeds int64")
		}
		totals[date] += row.Points
	}
	result := make([]gift.EarningsBucket, 0, len(totals))
	for date, points := range totals {
		result = append(result, gift.EarningsBucket{Date: date, Points: points})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Date > result[j].Date })
	return result, nil
}

func (s *Store) EarningsDetails(ctx context.Context, f gift.EarningsFilter, page, size int64) ([]gift.EarningsLine, int64, int64, error) {
	lines, err := s.earningOrders(ctx, f, false)
	if err != nil {
		return nil, 0, 0, err
	}
	sort.Slice(lines, func(i, j int) bool {
		if lines[i].CreatedAt.Equal(lines[j].CreatedAt) {
			return lines[i].ID > lines[j].ID
		}
		return lines[i].CreatedAt.After(lines[j].CreatedAt)
	})
	count := int64(len(lines))
	start := min(count, (page-1)*size)
	return lines[start:min(count, start+size)], page, count, nil
}
