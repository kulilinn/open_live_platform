package subscription

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"open_live_platform/internal/dm"
)

type Memory struct {
	memoryDirectory
	mu     sync.Mutex
	dm     *dm.Memory
	nextID int64
	plans  map[int64]Plan
	subs   map[[2]int64]Relationship
}

func NewMemory(messages *dm.Memory) *Memory {
	return &Memory{dm: messages, plans: make(map[int64]Plan), subs: make(map[[2]int64]Relationship)}
}

func (m *Memory) EnsureFreePlan(ctx context.Context, anchorID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.freePlan(anchorID).ID == 0 {
		m.createPlan(anchorID, Free, PlanInput{})
	}
	return nil
}

func (m *Memory) CreatePlan(ctx context.Context, anchorID int64, planType int, input PlanInput) (Plan, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Plan{}, err
	}
	return m.createPlan(anchorID, planType, input), nil
}

func (m *Memory) createPlan(anchorID int64, planType int, input PlanInput) Plan {
	m.nextID++
	now := time.Now().UTC()
	plan := Plan{ID: m.nextID, AnchorID: anchorID, Published: 1, Type: planType, Name: input.Name,
		URL: input.URL, Remark: input.Remark, Price: input.Price, CreatedAt: now, UpdatedAt: now}
	m.plans[plan.ID] = plan
	return plan
}

func (m *Memory) ChangePlan(ctx context.Context, anchorID, planID int64, change PlanChange) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	plan, ok := m.plans[planID]
	if !ok || plan.AnchorID != anchorID || plan.Deleted != 0 {
		return ErrNotFound
	}
	if change.Delete {
		plan.Deleted = 1
	}
	if change.Published != nil {
		plan.Published = 0
		if *change.Published {
			plan.Published = 1
		}
	}
	if fields := change.Fields; fields != nil {
		if plan.Type == Free {
			return ErrInvalidPlan
		}
		plan.Name, plan.URL, plan.Remark, plan.Price = fields.Name, fields.URL, fields.Remark, fields.Price
	}
	plan.UpdatedAt = time.Now().UTC()
	if err := plan.ApplyOffers(change, plan.UpdatedAt); err != nil {
		return err
	}
	m.plans[planID] = plan
	return nil
}

func (m *Memory) Plans(ctx context.Context, anchorID int64, includeHidden bool) ([]Plan, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := []Plan{}
	now := time.Now()
	for _, plan := range m.plans {
		if plan.AnchorID != anchorID || (!includeHidden && (plan.Deleted != 0 || plan.Published == 0)) {
			continue
		}
		for _, sub := range m.subs {
			if sub.PlanID == plan.ID && sub.Active(now) {
				plan.Count++
			}
		}
		result = append(result, plan.VisiblePromotion(now))
	}
	slices.SortFunc(result, func(a, b Plan) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return result, nil
}

func (m *Memory) CanCreatePlan(ctx context.Context, anchorID int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	now := time.Now()
	for _, sub := range m.subs {
		if sub.AnchorID == anchorID && m.plans[sub.PlanID].Deleted != 0 && sub.ExpiresAt.After(now) {
			return false, nil
		}
	}
	return true, nil
}

func (m *Memory) SetFreeSubscriptionOpen(ctx context.Context, anchorID int64, open bool) error {
	m.mu.Lock()
	plan := m.freePlan(anchorID)
	m.mu.Unlock()
	return m.ChangePlan(ctx, anchorID, plan.ID, PlanChange{Published: &open})
}

func (m *Memory) freePlan(anchorID int64) Plan {
	for _, plan := range m.plans {
		if plan.AnchorID == anchorID && plan.Type == Free {
			return plan
		}
	}
	return Plan{}
}

func (m *Memory) Relationships(ctx context.Context, fanID, anchorID int64) ([]Relationship, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	now := time.Now()
	result := []Relationship{}
	for _, sub := range m.subs {
		if (fanID == 0 || sub.FanID == fanID) && (anchorID == 0 || sub.AnchorID == anchorID) {
			result = append(result, sub.View(m.plans[sub.PlanID], now))
		}
	}
	slices.SortFunc(result, func(a, b Relationship) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return result, nil
}

func (m *Memory) SubscribeFree(ctx context.Context, members dm.Metadata) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if members.Fan.ID == members.Anchor.ID || !members.Anchor.Anchor {
		return ErrForbidden
	}
	plan := m.freePlan(members.Anchor.ID)
	if plan.ID == 0 || plan.Deleted != 0 || plan.Published == 0 {
		return ErrNotFound
	}
	now := time.Now().UTC()
	key := [2]int64{members.Fan.ID, plan.ID}
	sub := m.subs[key]
	if !sub.Active(now) {
		sub = NewRelationship(members.Fan.ID, plan, now, FreeExpiry(now))
	}
	return m.applyRelationship(ctx, members, key, sub, now)
}

func (m *Memory) SubscribeTrial(ctx context.Context, members dm.Metadata, planID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if members.Fan.ID == members.Anchor.ID || !members.Anchor.Anchor {
		return ErrForbidden
	}
	plan, found := m.plans[planID]
	if !found || plan.AnchorID != members.Anchor.ID {
		return ErrNotFound
	}
	key := [2]int64{members.Fan.ID, planID}
	if _, found := m.subs[key]; found {
		return ErrTrialClaimed
	}
	now := time.Now().UTC()
	expires, err := plan.TrialExpiry(now)
	if err != nil {
		return err
	}
	sub := NewRelationship(members.Fan.ID, plan, now, expires)
	sub.PromotionPurchased, sub.First = 1, 2
	return m.applyRelationship(ctx, members, key, sub, now)
}

func (m *Memory) Cancel(ctx context.Context, members dm.Metadata, planID int64, reason int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	key := [2]int64{members.Fan.ID, planID}
	sub, found := m.subs[key]
	if !found || sub.AnchorID != members.Anchor.ID {
		return ErrNotFound
	}
	now := time.Now().UTC()
	if sub.CancelledAt == nil {
		sub.CancelledAt, sub.Reason = &now, reason
		if sub.Level == 0 {
			sub.ExpiresAt = now
		}
	}
	return m.applyRelationship(ctx, members, key, sub, now)
}

func (m *Memory) applyRelationship(ctx context.Context, members dm.Metadata, key [2]int64, candidate Relationship, now time.Time) error {
	var until time.Time
	if candidate.Active(now) {
		until = candidate.ExpiresAt
	}
	for otherKey, sub := range m.subs {
		samePair := (sub.FanID == candidate.FanID && sub.AnchorID == candidate.AnchorID) || (sub.FanID == candidate.AnchorID && sub.AnchorID == candidate.FanID)
		if otherKey != key && samePair && sub.Active(now) && sub.ExpiresAt.After(until) {
			until = sub.ExpiresAt
		}
	}
	_, err := m.dm.SetDMSubscriptionUntil(ctx, members, !until.IsZero(), members.Welcome, until)
	if err != nil && !(until.IsZero() && errors.Is(err, dm.ErrNotFound)) {
		return err
	}
	m.subs[key] = candidate
	return nil
}

// Real debits require durable reservation and reconciliation history.
func (*Memory) ReservePurchase(context.Context, int64, int64, int64, PurchaseRequest) (Purchase, error) {
	return Purchase{}, ErrPurchaseReview
}

func (*Memory) DeclinePurchase(context.Context, string) error { return ErrPurchaseReview }

func (*Memory) ConfirmPurchase(context.Context, string, dm.Metadata, Charge) error {
	return ErrPurchaseReview
}
