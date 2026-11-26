package admin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/xuri/excelize/v2"
	"open_live_platform/internal/gift"
)

// LiveMedia publishes these four sheets with a header and fixed column positions.
func readGiftSheet(ctx context.Context, kind string, src io.Reader, legacyReader string) (GiftSheetBatch, error) {
	var batch GiftSheetBatch
	if !slices.Contains([]string{"gift_config", "gift_box", "quick_gift_config", "product_limit_config"}, kind) {
		return batch, errors.New("unknown workbook name")
	}
	rows, oldDate, closeBook, err := openGiftSheet(ctx, src, legacyReader)
	if err != nil {
		return batch, err
	}
	defer closeBook()
	return parseGiftRows(kind, rows, oldDate)
}

func parseGiftRows(kind string, rows iter.Seq2[[]string, error], oldDate bool) (GiftSheetBatch, error) {
	var batch GiftSheetBatch
	count := 0
	n := 0
	for raw, err := range rows {
		n++
		if err != nil {
			return batch, err
		}
		if n == 1 || len(raw) == 0 {
			continue
		}
		r := sheetRow{cells: raw, oldDate: oldDate}
		switch kind {
		case "gift_config":
			price := r.money(3)
			item := gift.Gift{ID: r.num(0, 64), GiftType: int8(r.num(1, 8)), Name: r.text(2), Price: price, Icon: r.text(4),
				Animation: r.str(5), NormalEffects: r.str(6), SpecialEffects: r.str(7), VibrationIntensity: int8(r.num(8, 8)), ToyInspection: int8(r.num(9, 8)),
				IsCustom: r.int8ptr(10), RecommendedDescription: r.str(11), ParameterOne: r.str(12), ParameterTwo: r.int32ptr(13), ParameterThree: r.int64ptr(14),
				IncomeStream: int32(r.num(15, 32)), PerformanceRule: int8(r.num(16, 8)), PerformanceTime: int32(r.num(17, 32)), Combo: int8(r.num(18, 8)), HardwareQueueType: int8(r.num(19, 8)),
				Announcement: r.str(20), AnnouncementOne: r.str(21), AnnouncementTwo: r.str(22), AnnouncementThree: r.str(23), EffectControlType: int8(r.num(24, 8)), GiftSort: r.int32ptr(25), BroadcastConfigID: r.int64ptr(26), HardwareBottomFrame: r.str(27)}
			if item.ID <= 0 || item.Name == "" || item.Price < 0 || item.IncomeStream < 0 || int64(item.IncomeStream) > item.Price {
				return batch, fmt.Errorf("row %d: invalid gift identity, price or income", n)
			}
			batch.Gifts = append(batch.Gifts, item)
		case "gift_box":
			batch.Boxes = append(batch.Boxes, gift.Box{ID: r.num(0, 64), Name: r.text(1), LimitIDs: r.text(2)})
		case "quick_gift_config":
			batch.Shortcuts = append(batch.Shortcuts, gift.QuickGift{ID: r.num(0, 64), LimitID: r.num(1, 64), PromotionID: r.num(2, 64), Sort: int32(r.num(3, 32))})
		case "product_limit_config":
			batch.Limits = append(batch.Limits, gift.Limit{ID: r.num(0, 64), GiftID: r.num(1, 64), PromotionID: r.num(2, 64),
				PerUser: r.num(3, 32), Platform: r.num(4, 32), DiscountType: int8(r.num(5, 8)), Income: r.int64ptr(6), Price: r.num(7, 32), Discount: int32(r.num(8, 32)), Listing: int8(r.num(9, 8)), Starts: r.date(10), Ends: r.date(11)})
		}
		if r.err != nil {
			return batch, fmt.Errorf("row %d: %w", n, r.err)
		}
		count++
	}
	if count == 0 {
		return batch, errors.New("Sheet1 contains no records")
	}
	return batch, nil
}

type sheetRow struct {
	cells   []string
	oldDate bool
	err     error
}

func (r sheetRow) text(i int) string {
	if i < len(r.cells) {
		return r.cells[i]
	}
	return ""
}
func (r sheetRow) str(i int) *string {
	if i >= len(r.cells) {
		return nil
	}
	return &r.cells[i]
}
func (r *sheetRow) invalid(i int, kind string) {
	if r.err == nil {
		r.err = fmt.Errorf("expected %s in column %d", kind, i+1)
	}
}
func (r *sheetRow) num(i, bits int) int64 {
	s := strings.TrimSpace(r.text(i))
	if s == "" {
		return 0
	}
	n, err := strconv.ParseInt(s, 10, bits)
	if err != nil {
		r.invalid(i, "an integer")
	}
	return n
}
func (r *sheetRow) int8ptr(i int) *int8 {
	if r.str(i) == nil {
		return nil
	}
	n := int8(r.num(i, 8))
	return &n
}
func (r *sheetRow) int32ptr(i int) *int32 {
	if r.str(i) == nil {
		return nil
	}
	n := int32(r.num(i, 32))
	return &n
}
func (r *sheetRow) int64ptr(i int) *int64 {
	if r.str(i) == nil {
		return nil
	}
	n := r.num(i, 64)
	return &n
}
func (r *sheetRow) money(i int) int64 {
	var p giftPrice
	if p.UnmarshalJSON([]byte(r.text(i))) != nil {
		r.invalid(i, "a whole point amount")
	}
	return int64(p)
}
func (r *sheetRow) date(i int) time.Time {
	s := strings.TrimSpace(r.text(i))
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		if t, err := excelize.ExcelDateToTime(n, r.oldDate); err == nil {
			return t.UTC()
		}
	}
	r.invalid(i, "a date")
	return time.Time{}
}
