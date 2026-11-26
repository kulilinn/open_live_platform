package subscription

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"math"
	"slices"
	"time"
)

type Bundle struct {
	ID       int64   `json:"id,string"`
	Days     int     `json:"promotion_time"`
	Discount float64 `json:"discount"`
	Price    float64 `json:"price"`
}

type OfferInput struct {
	Days     int     `json:"promotion_time"`
	Discount float64 `json:"discount"`
	Remark   string  `json:"remark"`
}

type Promotion struct {
	OfferInput
	ID        int64     `json:"id,string"`
	PlanID    int64     `json:"plan_id,string"`
	ExpiresAt time.Time `json:"-"`
}

func ValidOffer(days int, discount float64, now time.Time) bool {
	return days > 0 && days < 3_650_000 && now.AddDate(0, 0, days).Year() <= 9999 &&
		!math.IsNaN(discount) && discount >= 0 && discount <= 1
}

func NewOfferID() int64 {
	var random [8]byte
	rand.Read(random[:])
	return int64(binary.BigEndian.Uint64(random[:])&(1<<62-1)) + 1
}

func (p Plan) VisiblePromotion(now time.Time) Plan {
	if p.PromotionEndsAt == nil || !p.PromotionEndsAt.After(now) {
		p.Promote, p.Promotion = "", 0
	} else if p.Promote != "" {
		p.Promotion = 1
	}
	return p
}

func (p *Plan) ApplyOffers(change PlanChange, now time.Time) error {
	if p.Type == Free && (change.Bundles != nil || change.RemoveBundleID != 0 || change.Offer != nil || change.EndOffer) {
		return ErrInvalidPlan
	}
	switch {
	case change.Bundles != nil:
		value, err := json.Marshal(change.Bundles)
		if err != nil {
			return err
		}
		p.Bundle = string(value)
	case change.RemoveBundleID != 0:
		if p.Bundle == "" {
			return ErrNotFound
		}
		var bundles []Bundle
		if err := json.Unmarshal([]byte(p.Bundle), &bundles); err != nil {
			return err
		}
		bundles = slices.DeleteFunc(bundles, func(b Bundle) bool { return b.ID == change.RemoveBundleID })
		value, err := json.Marshal(bundles)
		if err != nil {
			return err
		}
		p.Bundle = string(value)
	}
	if change.EndOffer {
		p.Promote, p.PromotionEndsAt = "", nil
	}
	if offer := change.Offer; offer != nil && (p.PromotionEndsAt == nil || !p.PromotionEndsAt.After(now)) {
		value, err := json.Marshal(offer)
		if err != nil {
			return err
		}
		p.Promote, p.PromotionEndsAt = string(value), &offer.ExpiresAt
	}
	return nil
}

func (p Plan) TrialExpiry(now time.Time) (time.Time, error) {
	if p.Type == Free || p.Published != 1 || p.Deleted != 0 || p.VisiblePromotion(now).Promotion != 1 {
		return time.Time{}, ErrNotFound
	}
	var promotion Promotion
	if err := json.Unmarshal([]byte(p.Promote), &promotion); err != nil {
		return time.Time{}, err
	}
	if !ValidOffer(promotion.Days, promotion.Discount, now) {
		return time.Time{}, ErrInvalidPlan
	}
	return now.AddDate(0, 0, promotion.Days), nil
}
