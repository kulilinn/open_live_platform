package gift

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Question struct {
	ID          int64      `json:"id"`
	Subject     string     `json:"subject"`
	Target      int8       `json:"target"`
	AnswerOne   *string    `json:"answer_one"`
	AnswerTwo   *string    `json:"answer_two"`
	AnswerThree *string    `json:"answer_three"`
	AnswerFour  *string    `json:"answer_four"`
	AnswerFive  *string    `json:"answer_five"`
	AnswerSix   *string    `json:"answer_six"`
	AnswerSeven *string    `json:"answer_seven"`
	AnswerEight *string    `json:"answer_eight"`
	AnswerNine  *string    `json:"answer_nine"`
	AnswerTen   *string    `json:"answer_ten"`
	Deleted     int8       `json:"del_flag"`
	CreatedBy   *int64     `json:"create_by"`
	CreatedAt   *time.Time `json:"create_time"`
	UpdatedBy   *int64     `json:"update_by"`
	UpdatedAt   *time.Time `json:"update_time"`
}

func (q Question) Allows(answer string) bool {
	for _, choice := range []*string{q.AnswerOne, q.AnswerTwo, q.AnswerThree, q.AnswerFour, q.AnswerFive, q.AnswerSix,
		q.AnswerSeven, q.AnswerEight, q.AnswerNine, q.AnswerTen} {
		if choice != nil && *choice == answer {
			return true
		}
	}
	return false
}

type Promotion struct {
	ID          int64     `json:"id,string"`
	GiftID      int64     `json:"productId,string"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Starts      time.Time `json:"startDate"`
	Ends        time.Time `json:"endDate"`
}

func (p Promotion) Active(now time.Time) bool { return !now.Before(p.Starts) && now.Before(p.Ends) }

type Limit struct {
	ID            int64           `json:"id,string"`
	GiftID        int64           `json:"productId,string"`
	PromotionID   int64           `json:"promotionId,string"`
	Starts        time.Time       `json:"limitStartDate"`
	Ends          time.Time       `json:"limitEndDate"`
	Price         int64           `json:"discountPrice"`
	Income        *int64          `json:"discountIncome"`
	DiscountType  int8            `json:"discountType"`
	Discount      int32           `json:"discountValue"`
	PerUser       int64           `json:"maxPurchasePerUser"`
	Platform      int64           `json:"maxPurchasePlatform"`
	Listing       int8            `json:"listingStatus"`
	UserUsage     map[int64]int64 `json:"usedByUser,omitempty"`
	PlatformUsage *int64          `json:"usedPlatform,omitempty"`
}

func (l Limit) Active(now time.Time) bool { return !now.Before(l.Starts) && now.Before(l.Ends) }

type Box struct {
	ID       int64  `json:"id,string"`
	Name     string `json:"name"`
	LimitIDs string `json:"giftIds"`
}

type Interactions struct {
	Questions  []Question  `json:"questions"`
	Promotions []Promotion `json:"promotions"`
	Limits     []Limit     `json:"limits"`
	Boxes      []Box       `json:"boxes"`
	QuickGifts []QuickGift `json:"quick_gifts"`
}

type QuickGift struct {
	ID          int64 `json:"id,string"`
	LimitID     int64 `json:"limit_id,string"`
	PromotionID int64 `json:"promotion_id,string"`
	Sort        int32 `json:"gift_sort"`
}

type PromotionView struct {
	GiftID      int64  `json:"gift_id,string"`
	LimitID     int64  `json:"limit_id,string"`
	Name        string `json:"gift_name"`
	Icon        string `json:"gift_icon"`
	Sort        int32  `json:"gift_sort"`
	Type        int8   `json:"gift_type"`
	Duration    int64  `json:"vibration_time"`
	Vibration   int8   `json:"vibration_intensity"`
	Control     int8   `json:"effect_control_type"`
	Combo       int8   `json:"combo"`
	Custom      *int8  `json:"is_custom"`
	Original    int64  `json:"original_price"`
	Discounted  int64  `json:"discount_price"`
	Discount    int32  `json:"discount_value"`
	Promotion   string `json:"promotions_name"`
	QuickGiftID int64  `json:"quick_gift_id,string"`
}

func LoadInteractions(path string) (Interactions, error) {
	var result Interactions
	if path == "" {
		return result, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return result, err
	}
	ids := map[int64]struct{}{}
	for _, question := range result.Questions {
		if _, dup := ids[question.ID]; dup || question.ID <= 0 || question.Target < 1 || question.Target > 2 || question.Subject == "" {
			return result, fmt.Errorf("invalid gift game question %d", question.ID)
		}
		ids[question.ID] = struct{}{}
	}
	for _, limit := range result.Limits {
		if limit.ID <= 0 || limit.GiftID <= 0 || limit.Price < 0 || limit.PerUser < 0 || limit.Platform < 0 ||
			(limit.Income != nil && *limit.Income < 0) ||
			limit.PlatformUsage != nil && *limit.PlatformUsage < 0 {
			return result, fmt.Errorf("invalid gift limit %d", limit.ID)
		}
		for _, count := range limit.UserUsage {
			if count < 0 {
				return result, fmt.Errorf("invalid usage for gift limit %d", limit.ID)
			}
		}
	}
	return result, nil
}

func (i Interactions) ActiveQuestions() []Question {
	result := []Question{}
	for _, q := range i.Questions {
		if q.Deleted == 0 {
			result = append(result, q)
		}
	}
	return result
}

func (i Interactions) PromotionList(items []Gift, now time.Time) []PromotionView {
	result := make([]PromotionView, 0, len(items))
	for _, item := range items {
		promotion, _ := first(i.Promotions, func(p Promotion) bool { return p.GiftID == item.ID && p.Active(now) })
		limit, _ := first(i.Limits, func(l Limit) bool { return l.GiftID == item.ID && l.PromotionID == promotion.ID && l.Active(now) })
		// On the legacy, deprecated all-catalog view limit_id carries the gift ID.
		limit.ID = item.ID
		result = append(result, promotionView(item, promotion, limit))
	}
	sort.SliceStable(result, func(a, b int) bool { return result[a].Sort < result[b].Sort })
	return result
}

func (i Interactions) BoxContents(boxID, userID int64, items []Gift, now time.Time) []PromotionView {
	result := []PromotionView{}
	box, ok := first(i.Boxes, func(b Box) bool { return b.ID == boxID })
	if !ok {
		return result
	}
	seen := map[int64]struct{}{}
	for _, id := range strings.Split(box.LimitIDs, ",") {
		limitID, err := strconv.ParseInt(strings.TrimSpace(id), 10, 64)
		if err != nil {
			continue
		}
		if _, ok := seen[limitID]; ok {
			continue
		}
		seen[limitID] = struct{}{}
		if item, ok := i.PromotionAtLimit(limitID, userID, items, now); ok {
			result = append(result, item)
		}
	}
	sort.SliceStable(result, func(a, b int) bool { return result[a].Sort < result[b].Sort })
	return result
}

func (i Interactions) PromotionAtLimit(limitID, userID int64, items []Gift, now time.Time) (PromotionView, bool) {
	limit, ok := first(i.Limits, func(l Limit) bool { return l.ID == limitID && l.Listing == 1 && l.Active(now) })
	if !ok || (limit.PerUser > 0 && (limit.UserUsage == nil || limit.UserUsage[userID] >= limit.PerUser)) ||
		(limit.Platform > 0 && (limit.PlatformUsage == nil || *limit.PlatformUsage >= limit.Platform)) {
		return PromotionView{}, false
	}
	item, ok := first(items, func(g Gift) bool { return g.ID == limit.GiftID })
	if !ok {
		return PromotionView{}, false
	}
	promotion, _ := first(i.Promotions, func(p Promotion) bool { return p.ID == limit.PromotionID && p.GiftID == item.ID })
	return promotionView(item, promotion, limit), true
}

func first[T any](values []T, pred func(T) bool) (T, bool) {
	if n := slices.IndexFunc(values, pred); n >= 0 {
		return values[n], true
	}
	var zero T
	return zero, false
}

func promotionView(g Gift, p Promotion, l Limit) PromotionView {
	var duration int64
	var order int32
	if g.ParameterOne != nil {
		duration, _ = strconv.ParseInt(*g.ParameterOne, 10, 64)
	}
	if g.GiftSort != nil {
		order = *g.GiftSort
	}
	original := g.Price
	if original > math.MaxInt32 || original < math.MinInt32 {
		original = 0
	}
	return PromotionView{GiftID: g.ID, LimitID: l.ID, Name: g.Name, Icon: g.Icon, Sort: order, Type: g.GiftType, Duration: duration,
		Vibration: g.VibrationIntensity, Control: g.EffectControlType, Combo: g.Combo, Custom: g.IsCustom,
		Original: original, Discounted: l.Price, Discount: l.Discount, Promotion: p.Name}
}
