package subscription

import (
	"context"
	"crypto/md5"
	"errors"
	"fmt"
	"time"

	"open_live_platform/internal/dm"
)

var (
	ErrNotFound          = errors.New("subscription plan or relationship not found")
	ErrForbidden         = errors.New("subscription action is forbidden")
	ErrInvalidPlan       = errors.New("invalid subscription plan")
	ErrPurchaseReview    = errors.New("subscription payment is pending verification")
	ErrTrialClaimed      = errors.New("subscription promotion already claimed")
	ErrRenewalIneligible = errors.New("subscription is ineligible for renewal")
)

const (
	Free     = 2
	Basic    = 3
	Advanced = 4
)

type Plan struct {
	ID              int64      `json:"id,string"`
	AnchorID        int64      `json:"anchor_id,string"`
	Published       int        `json:"is_pub"`
	Deleted         int        `json:"is_del"`
	Promotion       int        `json:"is_promotion"`
	Type            int        `json:"ty"`
	Bundle          string     `json:"buddle"`
	URL             string     `json:"url"`
	Remark          string     `json:"remark"`
	Price           int64      `json:"price"`
	Promote         string     `json:"promotion"`
	CreatedAt       time.Time  `json:"create_at"`
	UpdatedAt       time.Time  `json:"update_at"`
	Name            string     `json:"name"`
	Count           int        `json:"sub_num"`
	PromotionEndsAt *time.Time `json:"-"`
}

type PlanInput struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Remark string `json:"remark"`
	Price  int64  `json:"price"`
}

type PlanChange struct {
	Fields         *PlanInput
	Published      *bool
	Delete         bool
	Bundles        []Bundle
	RemoveBundleID int64
	Offer          *Promotion
	EndOffer       bool
}

type Relationship struct {
	PlanPrice          int64      `json:"-"`
	ID                 string     `json:"id"`
	FanID              int64      `json:"uid,string"`
	AnchorID           int64      `json:"anchor_id,string"`
	PlanID             int64      `json:"plan_id,string"`
	PlanName           string     `json:"plan_name"`
	Bundle             string     `json:"buddle"`
	CreatedAt          time.Time  `json:"create_at"`
	ExpiresAt          time.Time  `json:"expire_at"`
	PromotionPurchased int        `json:"is_promotion_purchased"`
	First              int        `json:"first_sub"`
	Expired            int        `json:"is_expired"`
	Level              int        `json:"subscription_level"`
	CancelledAt        *time.Time `json:"cancel_at"`
	Reason             int        `json:"reason"`
}

func (r Relationship) Active(now time.Time) bool {
	return r.CancelledAt == nil && r.ExpiresAt.After(now)
}

func (r Relationship) View(plan Plan, now time.Time) Relationship {
	r.PlanName = plan.Name
	r.PlanPrice = plan.Price
	if r.First == 0 {
		r.First = 1
	}
	if !r.ExpiresAt.After(now) {
		r.Expired = 1
	}
	return r
}

func RelationshipID(fanID, anchorID, planID int64) string {
	value := fmt.Sprintf("%d:%d:%d", anchorID, fanID, planID)
	return fmt.Sprintf("%x", md5.Sum([]byte(value)))
}

func NewRelationship(fanID int64, plan Plan, now, expires time.Time) Relationship {
	return Relationship{ID: RelationshipID(fanID, plan.AnchorID, plan.ID), FanID: fanID, AnchorID: plan.AnchorID, PlanID: plan.ID,
		CreatedAt: now, ExpiresAt: expires, Level: plan.Type - Free}
}

func FreeExpiry(now time.Time) time.Time { return now.AddDate(0, 0, 365*10) }

func PaidExpiry(now time.Time) time.Time {
	now = now.UTC()
	first := time.Date(now.Year(), now.Month()+1, 1, 23, 59, 59, 0, time.UTC)
	last := first.AddDate(0, 1, -1)
	day := min(now.Day(), last.Day())
	if now.AddDate(0, 0, 1).Day() == 1 {
		day = last.Day()
	}
	return first.AddDate(0, 0, day-1)
}

type Purchase struct {
	OrderNo  string
	FanID    int64
	AnchorID int64
	PlanID   int64
	Points   int64
	New      bool
	Paid     bool
}

type Charge struct {
	OrderID       int64
	OrderNo       string
	TotalPoints   int64
	BadDebtPoints int64
}

type RenewalCandidate struct {
	FanID, AnchorID, PlanID int64
	ExpiresAt               time.Time
}

type RenewalStore interface {
	RenewalCandidates(ctx context.Context, cutoff time.Time) ([]RenewalCandidate, error)
	ReserveRenewal(ctx context.Context, candidate RenewalCandidate, orderNo string) (Purchase, error)
	DeclineRenewal(ctx context.Context, orderNo string, members dm.Metadata) error
}

type PendingRenewal struct{ OrderNo string }

func (p PendingRenewal) Error() string { return ErrPurchaseReview.Error() }
func (p PendingRenewal) Unwrap() error { return ErrPurchaseReview }

type Store interface {
	DirectoryStore
	EnsureFreePlan(ctx context.Context, anchorID int64) error
	CreatePlan(ctx context.Context, anchorID int64, planType int, input PlanInput) (Plan, error)
	ChangePlan(ctx context.Context, anchorID, planID int64, change PlanChange) error
	Plans(ctx context.Context, anchorID int64, includeHidden bool) ([]Plan, error)
	CanCreatePlan(ctx context.Context, anchorID int64) (bool, error)
	SetFreeSubscriptionOpen(ctx context.Context, anchorID int64, open bool) error
	Relationships(ctx context.Context, fanID, anchorID int64) ([]Relationship, error)
	SubscribeFree(ctx context.Context, members dm.Metadata) error
	SubscribeTrial(ctx context.Context, members dm.Metadata, planID int64) error
	Cancel(ctx context.Context, members dm.Metadata, planID int64, reason int) error
	ReservePurchase(ctx context.Context, fanID, anchorID, planID int64, orderNo string) (Purchase, error)
	DeclinePurchase(ctx context.Context, orderNo string) error
	ConfirmPurchase(ctx context.Context, orderNo string, members dm.Metadata, charge Charge) error
}
