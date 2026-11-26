package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/dm"
	"open_live_platform/internal/feed"
	"open_live_platform/internal/live"
	"open_live_platform/internal/payment"
	"open_live_platform/internal/posts"
	"open_live_platform/internal/subscription"
	"open_live_platform/internal/transport/httpx"
)

type subscriptionAPI struct {
	server
	store   subscription.Store
	inbox   feed.Store
	billing *payment.Client
}

func RegisterSubscriptions(mux *http.ServeMux, accounts auth.AccountStore, rooms live.RoomStore, store subscription.Store, billing *payment.Client, inbox feed.Store, renewalToken string, reviewer ...posts.Reviewer) {
	s := subscriptionAPI{server: server{accounts: accounts, rooms: rooms}, store: store, billing: billing, inbox: inbox}
	if len(reviewer) > 0 {
		s.staticReviewer = reviewer[0]
	}
	for path, handler := range map[string]userHandler{
		"POST /api/sub/anchor/create_free":     s.createFreePlan,
		"POST /api/sub/anchor/create_pay":      s.createPaidPlan(subscription.Basic),
		"POST /api/sub/anchor/create_1v1":      s.createPaidPlan(subscription.Advanced),
		"POST /api/sub/anchor/pub":             s.changePlan("publish"),
		"POST /api/sub/anchor/unpub":           s.changePlan("unpublish"),
		"POST /api/sub/anchor/edit":            s.changePlan("edit"),
		"POST /api/sub/anchor/del":             s.changePlan("delete"),
		"POST /api/sub/anchor/can_create_plan": s.canCreatePlan,
		"POST /api/sub/anchor/set_free_sub":    s.setFreePlan,
		"POST /api/sub/anchor/promote":         s.promotePlan,
		"POST /api/sub/anchor/unpromote":       s.stopPromotion,
		"POST /api/sub/anchor/create_buddle":   s.saveBundles,
		"POST /api/sub/anchor/del_buddle":      s.deleteBundle,
		"POST /api/sub/get_anchor_plan":        s.listPlans,
		"POST /api/sub/free_sub":               s.subscribeFree,
		"POST /api/sub/pay_sub":                s.subscribePaid,
		"GET /api/sub/cancel_sub":              s.cancelSubscription,
		"POST /api/sub/get_sub":                s.getSubscriptions,
		"POST /api/sub/get_my_sub":             s.getAnchorSubscriptions,
		"POST /api/sub/sub_list":               s.subscriptionList,
		"GET /api/sub/fans/subscription_list":  s.subscriptionPage(false),
		"GET /api/sub/anchor/fans_list":        s.subscriptionPage(true),
		"POST /api/sub/get_groups":             s.groups,
		"POST /api/sub/create_group":           s.createGroup,
		"POST /api/sub/rename_group":           s.changeGroup("rename"),
		"POST /api/sub/clear_group":            s.changeGroup("clear"),
		"POST /api/sub/del_group":              s.changeGroup("delete"),
		"POST /api/sub/add_group":              s.changeGroupMember(true),
		"POST /api/sub/remove_group":           s.changeGroupMember(false),
		"POST /api/sub/anchor_list":            s.listAnchors,
		"POST /api/sub/friend_list":            s.savedUsers(subscription.Friends),
		"POST /api/sub/blacklist":              s.savedUsers(subscription.Blacklist),
		"GET /api/sub/fans_pren":               s.publicFanPage,
		"GET /api/sub/fans_level/{anchor_id}":  s.fanLevel,
		"GET /api/sub/anchor/fans_statistics":  s.subscriptionStatistics(true),
		"GET /api/sub/sub_statistics":          s.subscriptionStatistics(false),
		"GET /api/sub/level/{room}":            s.roomSubscriptionLevel,
		"POST /api/sub/index":                  s.subscriptionIndex,
		"POST /api/sub/upgrade":                s.legacyPlanChange,
		"POST /api/sub/downgrade":              s.legacyPlanChange,
	} {
		mux.HandleFunc(path, s.authenticated(handler))
	}
	s.registerRenewal(mux, renewalToken)
}

type subscriptionID int64

func (id *subscriptionID) UnmarshalJSON(value []byte) error {
	var raw string
	if len(value) > 0 && value[0] == '"' {
		if err := json.Unmarshal(value, &raw); err != nil {
			return err
		}
	} else {
		raw = string(value)
	}
	number, err := strconv.ParseInt(raw, 10, 64)
	if err == nil && number < 0 {
		err = subscription.ErrInvalidPlan
	}
	*id = subscriptionID(number)
	return err
}

func (s subscriptionAPI) anchor(ctx context.Context, id int64) (auth.User, error) {
	user, found, err := s.accounts.Find(ctx, id)
	if err != nil {
		return auth.User{}, err
	}
	if !found {
		return auth.User{}, subscription.ErrNotFound
	}
	if user.Profile != nil {
		if user.Profile.IsAnchor {
			return user, nil
		}
		return auth.User{}, subscription.ErrForbidden
	}
	room, err := s.rooms.Info(ctx, id)
	if errors.Is(err, live.ErrRoomNotFound) || (err == nil && room.OwnerID != id) {
		return auth.User{}, subscription.ErrForbidden
	}
	return user, err
}

func (s subscriptionAPI) requireAnchor(w http.ResponseWriter, r *http.Request, id int64) bool {
	_, err := s.anchor(r.Context(), id)
	if err == nil {
		return true
	}
	subscriptionError(w, err)
	return false
}

func (s subscriptionAPI) createFreePlan(w http.ResponseWriter, r *http.Request, actor int64) {
	if s.requireAnchor(w, r, actor) {
		subscriptionStatus(w, s.store.EnsureFreePlan(r.Context(), actor))
	}
}

func (s subscriptionAPI) createPaidPlan(planType int) userHandler {
	return func(w http.ResponseWriter, r *http.Request, actor int64) {
		if !s.requireAnchor(w, r, actor) {
			return
		}
		var input subscription.PlanInput
		if !decodeEmailRequest(w, r, &input) || !validPaidPlan(w, input) {
			return
		}
		allowed, err := s.store.CanCreatePlan(r.Context(), actor)
		if err != nil {
			subscriptionError(w, err)
			return
		}
		if !allowed {
			subscriptionError(w, subscription.ErrForbidden)
			return
		}
		plan, err := s.store.CreatePlan(r.Context(), actor, planType, input)
		if err != nil {
			subscriptionError(w, err)
			return
		}
		httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "result": map[string]string{"plan_id": strconv.FormatInt(plan.ID, 10)}})
	}
}

func validPaidPlan(w http.ResponseWriter, input subscription.PlanInput) bool {
	if input.Price < 10 || input.Price > 1<<31-1 || utf8.RuneCountInString(input.Name) > 128 || len(input.URL) > 2048 || len(input.Remark) > 4096 {
		subscriptionError(w, subscription.ErrInvalidPlan)
		return false
	}
	return true
}

func (s subscriptionAPI) changePlan(action string) userHandler {
	return func(w http.ResponseWriter, r *http.Request, actor int64) {
		if !s.requireAnchor(w, r, actor) {
			return
		}
		var input struct {
			PlanID subscriptionID `json:"plan_id"`
			subscription.PlanInput
		}
		if !decodeEmailRequest(w, r, &input) {
			return
		}
		change := subscription.PlanChange{}
		ctx := r.Context()
		switch action {
		case "edit":
			if !validPaidPlan(w, input.PlanInput) {
				return
			}
			change.Fields = &input.PlanInput
			if s.staticReviewer != nil {
				plans, err := s.store.Plans(ctx, actor, true)
				if err != nil {
					subscriptionError(w, err)
					return
				}
				var previous *subscription.Plan
				for i := range plans {
					if plans[i].ID == int64(input.PlanID) && plans[i].Deleted == 0 {
						previous = &plans[i]
						break
					}
				}
				if previous == nil {
					subscriptionError(w, subscription.ErrNotFound)
					return
				}
				var ok bool
				ctx, ok = submitStaticReviews(w, ctx, s.staticReviewer, actor, staticContent{typeID: posts.ReviewPlanName,
					object: int64(input.PlanID), kind: "subscription_plan_name", before: previous.Name, after: input.Name, refusal: "4056"})
				if !ok {
					return
				}
			}
		case "delete":
			change.Delete = true
		default:
			published := action == "publish"
			change.Published = &published
		}
		subscriptionStatus(w, s.store.ChangePlan(ctx, actor, int64(input.PlanID), change))
	}
}

func (s subscriptionAPI) canCreatePlan(w http.ResponseWriter, r *http.Request, actor int64) {
	if !s.requireAnchor(w, r, actor) {
		return
	}
	allowed, err := s.store.CanCreatePlan(r.Context(), actor)
	if err != nil {
		subscriptionError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "result": map[string]bool{"can_create": allowed}})
}

func (s subscriptionAPI) setFreePlan(w http.ResponseWriter, r *http.Request, actor int64) {
	if !s.requireAnchor(w, r, actor) {
		return
	}
	var input struct{ Open *int }
	if !decodeEmailRequest(w, r, &input) {
		return
	}
	if input.Open == nil || (*input.Open != 0 && *input.Open != 1) {
		subscriptionError(w, subscription.ErrInvalidPlan)
		return
	}
	ctx := r.Context()
	if err := s.store.EnsureFreePlan(ctx, actor); err != nil {
		subscriptionError(w, err)
		return
	}
	subscriptionStatus(w, s.store.SetFreeSubscriptionOpen(ctx, actor, *input.Open == 1))
}

func (s subscriptionAPI) listPlans(w http.ResponseWriter, r *http.Request, actor int64) {
	var input struct {
		AnchorID subscriptionID `json:"anchor_id"`
	}
	if !decodeEmailRequest(w, r, &input) {
		return
	}
	anchorID := int64(input.AnchorID)
	includeHidden := anchorID == 0
	if includeHidden {
		anchorID = actor
	}
	ctx := r.Context()
	if _, err := s.anchor(ctx, anchorID); err == nil {
		if err := s.store.EnsureFreePlan(ctx, anchorID); err != nil {
			subscriptionError(w, err)
			return
		}
	} else if !errors.Is(err, subscription.ErrNotFound) && !errors.Is(err, subscription.ErrForbidden) {
		subscriptionError(w, err)
		return
	}
	plans, err := s.store.Plans(ctx, anchorID, includeHidden)
	if err != nil {
		subscriptionError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": plans})
}

func (s subscriptionAPI) members(ctx context.Context, fanID, anchorID int64) (dm.Metadata, error) {
	if fanID == anchorID || fanID < 1 || anchorID < 1 {
		return dm.Metadata{}, subscription.ErrForbidden
	}
	anchor, err := s.anchor(ctx, anchorID)
	if err != nil {
		return dm.Metadata{}, err
	}
	fan, found, err := s.accounts.Find(ctx, fanID)
	if err != nil {
		return dm.Metadata{}, err
	}
	if !found {
		return dm.Metadata{}, subscription.ErrNotFound
	}
	settings, err := s.inbox.FeedConfig(ctx, anchorID)
	return dm.Metadata{Fan: subscriptionDMUser(fan, false), Anchor: subscriptionDMUser(anchor, true), Welcome: settings.Welcome}, err
}

func subscriptionDMUser(user auth.User, anchor bool) dm.User {
	result := dm.User{ID: user.ID, DisplayName: user.Account, SearchName: user.Account, Anchor: anchor}
	if profile := user.Profile; profile != nil {
		result.Anchor = result.Anchor || profile.IsAnchor
		result.SearchName, result.Photo, result.ToyInfo = profile.SearchName, profile.Photo, profile.ToyInfo
	}
	return result
}

type subscribeInput struct {
	AnchorID  subscriptionID `json:"anchor_id"`
	PlanID    subscriptionID `json:"plan_id"`
	Bundle    string         `json:"buddle"`
	Promotion int            `json:"promotion"`
}

func (s subscriptionAPI) subscribeFree(w http.ResponseWriter, r *http.Request, actor int64) {
	var input subscribeInput
	if !decodeEmailRequest(w, r, &input) {
		return
	}
	ctx := r.Context()
	members, err := s.members(ctx, actor, int64(input.AnchorID))
	if err == nil {
		err = s.store.EnsureFreePlan(ctx, members.Anchor.ID)
	}
	if err == nil {
		err = s.store.SubscribeFree(ctx, members)
	}
	if err != nil {
		subscriptionError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "result": struct{}{}})
}

func (s subscriptionAPI) subscribePaid(w http.ResponseWriter, r *http.Request, actor int64) {
	var input subscribeInput
	if !decodeEmailRequest(w, r, &input) {
		return
	}
	if input.Promotion != 0 && input.Promotion != 1 {
		subscriptionError(w, subscription.ErrInvalidPlan)
		return
	}
	if input.Promotion == 0 && s.billing == nil {
		httpx.Error(w, http.StatusServiceUnavailable, "subscription billing is unavailable")
		return
	}
	ctx := r.Context()
	members, err := s.members(ctx, actor, int64(input.AnchorID))
	if err != nil {
		subscriptionError(w, err)
		return
	}
	if input.Promotion == 1 {
		subscriptionStatus(w, s.store.SubscribeTrial(ctx, members, int64(input.PlanID)))
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) > 128 || strings.IndexFunc(key, func(c rune) bool { return c <= ' ' || c >= 127 }) >= 0 {
		httpx.Error(w, http.StatusBadRequest, "invalid Idempotency-Key")
		return
	}
	order, err := s.store.ReservePurchase(ctx, actor, members.Anchor.ID, int64(input.PlanID),
		subscription.PurchaseRequest{OrderNo: "sub-" + rand.Text(), Key: key, Bundle: input.Bundle})
	if err != nil {
		subscriptionError(w, err)
		return
	}
	if order.Paid {
		subscriptionStatus(w, nil)
		return
	}
	if order.Declined {
		subscriptionPaymentDeclined(w)
		return
	}
	if !order.New {
		subscriptionPaymentPending(w, order.OrderNo)
		return
	}
	receipt, err := s.billing.Decrease(ctx, actor, order.Points, order.OrderNo)
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if errors.Is(err, payment.ErrInsufficientPoints) {
		if err := s.store.DeclinePurchase(finishCtx, order.OrderNo); err != nil {
			subscriptionPaymentPending(w, order.OrderNo)
			return
		}
		subscriptionPaymentDeclined(w)
		return
	}
	if err != nil {
		subscriptionPaymentPending(w, order.OrderNo)
		return
	}
	if err := s.store.ConfirmPurchase(finishCtx, order.OrderNo, members, subscriptionCharge(receipt)); err != nil {
		subscriptionPaymentPending(w, order.OrderNo)
		return
	}
	subscriptionStatus(w, nil)
}

func subscriptionPaymentDeclined(w http.ResponseWriter) {
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "4046", "message": "insufficient points"})
}

func (s subscriptionAPI) cancelSubscription(w http.ResponseWriter, r *http.Request, actor int64) {
	query := r.URL.Query()
	anchorID, anchorErr := strconv.ParseInt(query.Get("anchor_id"), 10, 64)
	planID, planErr := strconv.ParseInt(query.Get("plan_id"), 10, 64)
	reason, reasonErr := strconv.Atoi(query.Get("reason"))
	if anchorErr != nil || planErr != nil || reasonErr != nil || reason < 0 || reason > 7 || planID < 1 {
		subscriptionError(w, subscription.ErrInvalidPlan)
		return
	}
	ctx := r.Context()
	members, err := s.members(ctx, actor, anchorID)
	if err == nil {
		err = s.store.Cancel(ctx, members, planID, reason)
	}
	var pending subscription.PendingRenewal
	if errors.As(err, &pending) {
		subscriptionPaymentPending(w, pending.OrderNo)
		return
	}
	if err != nil {
		subscriptionError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": nil})
}

func subscriptionPaymentPending(w http.ResponseWriter, orderNo string) {
	httpx.JSON(w, http.StatusServiceUnavailable, map[string]string{"error": subscription.ErrPurchaseReview.Error(), "order_no": orderNo})
}

func subscriptionStatus(w http.ResponseWriter, err error) {
	if err != nil {
		subscriptionError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "200"})
}

func subscriptionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, subscription.ErrNotFound):
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "4005", "message": err.Error()})
	case errors.Is(err, subscription.ErrForbidden):
		httpx.Error(w, http.StatusForbidden, err.Error())
	case errors.Is(err, subscription.ErrInvalidPlan):
		httpx.Error(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, subscription.ErrTrialClaimed):
		httpx.JSON(w, http.StatusForbidden, map[string]string{"status": "4016", "message": err.Error()})
	case errors.Is(err, subscription.ErrPurchaseReview):
		httpx.Error(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, subscription.ErrPurchaseConflict):
		httpx.Error(w, http.StatusConflict, err.Error())
	default:
		httpx.Error(w, http.StatusInternalServerError, "could not update subscriptions")
	}
}
