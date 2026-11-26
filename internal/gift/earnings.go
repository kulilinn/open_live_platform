package gift

import (
	"context"
	"time"
)

const StreamGiftIncomeType int8 = 2

type EarningsFilter struct {
	AnchorID int64
	Type     *int8
	From, To time.Time
	Monthly  bool
}

type EarningsLine struct {
	ID          int64     `json:"id,string"`
	AnchorID    int64     `json:"anchor_id,string"`
	GiverID     int64     `json:"giver_id,string"`
	GiverName   string    `json:"giver_name"`
	Type        int8      `json:"income_type"`
	Points      int64     `json:"points"`
	CreatedAt   time.Time `json:"create_at"`
	Description string    `json:"income_details"`
}

type EarningsBucket struct {
	Date   string `json:"date"`
	Points int64  `json:"total_points,string"`
}

type EarningsStore interface {
	EarningsSummary(context.Context, EarningsFilter) ([]EarningsBucket, error)
	EarningsDetails(_ context.Context, _ EarningsFilter, page, size int64) (lines []EarningsLine, current, count int64, err error)
}

func NewEarningsFilter(anchorID int64, dateType int64, incomeType *int8, now time.Time) EarningsFilter {
	now = now.UTC()
	var start time.Time
	switch dateType {
	case 1:
		start = now.AddDate(0, 0, -7)
	case 2:
		start = now.AddDate(0, 0, -30)
	default:
		month := time.Date(now.Year(), now.Month()-6, 1, 0, 0, 0, 0, time.UTC)
		lastDay := month.AddDate(0, 1, -1).Day()
		start = month.AddDate(0, 0, min(now.Day(), lastDay)-1)
	}
	return EarningsFilter{AnchorID: anchorID, Type: incomeType, From: start, To: now, Monthly: dateType == 3}
}

func (f EarningsFilter) Bucket(at time.Time) string {
	layout := "2006-01-02"
	if f.Monthly {
		layout = "2006-01"
	}
	return at.UTC().Format(layout)
}

func (o Order) EarningsLine() EarningsLine {
	return EarningsLine{ID: o.ID, AnchorID: o.Input.ReceiverID, GiverID: o.Input.SenderID, GiverName: o.Sender.Name,
		Type: StreamGiftIncomeType, Points: o.Income, CreatedAt: o.PaidAt.UTC(), Description: o.Item.Name}
}
