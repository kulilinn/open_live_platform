package httpapi

import (
	"math"
	"net/http"
	"strconv"
	"time"

	"open_live_platform/internal/subscription"
	"open_live_platform/internal/transport/httpx"
)

func (s subscriptionAPI) promotePlan(w http.ResponseWriter, r *http.Request, ownerID int64) {
	if !s.requireAnchor(w, r, ownerID) {
		return
	}
	var input struct {
		PlanID subscriptionID          `json:"plan_id"`
		Offer  subscription.OfferInput `json:"promotion"`
	}
	if !decodeEmailRequest(w, r, &input) {
		return
	}
	now := time.Now().UTC()
	if input.PlanID < 1 || !subscription.ValidOffer(input.Offer.Days, input.Offer.Discount, now) || len(input.Offer.Remark) > 4096 {
		subscriptionError(w, subscription.ErrInvalidPlan)
		return
	}
	planID := int64(input.PlanID)
	offer := subscription.Promotion{OfferInput: input.Offer, ID: subscription.NewOfferID(), PlanID: planID,
		ExpiresAt: now.AddDate(0, 0, input.Offer.Days)}
	if err := s.store.ChangePlan(r.Context(), ownerID, planID, subscription.PlanChange{Offer: &offer}); err != nil {
		subscriptionError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "result": struct{}{}})
}

func (s subscriptionAPI) stopPromotion(w http.ResponseWriter, r *http.Request, ownerID int64) {
	if !s.requireAnchor(w, r, ownerID) {
		return
	}
	var input struct {
		PlanID subscriptionID `json:"plan_id"`
	}
	if decodeEmailRequest(w, r, &input) {
		subscriptionStatus(w, s.store.ChangePlan(r.Context(), ownerID, int64(input.PlanID), subscription.PlanChange{EndOffer: true}))
	}
}

func (s subscriptionAPI) saveBundles(w http.ResponseWriter, r *http.Request, ownerID int64) {
	if !s.requireAnchor(w, r, ownerID) {
		return
	}
	var input struct {
		PlanID  subscriptionID        `json:"plan_id"`
		Bundles []subscription.Bundle `json:"list"`
	}
	if !decodeEmailRequest(w, r, &input) {
		return
	}
	if input.PlanID < 1 || input.Bundles == nil {
		subscriptionError(w, subscription.ErrInvalidPlan)
		return
	}
	now := time.Now().UTC()
	ids := make([]string, len(input.Bundles))
	for i, bundle := range input.Bundles {
		if !subscription.ValidOffer(bundle.Days, bundle.Discount, now) || bundle.Price < 0 || math.IsInf(bundle.Price, 0) || math.IsNaN(bundle.Price) {
			subscriptionError(w, subscription.ErrInvalidPlan)
			return
		}
		input.Bundles[i].ID = subscription.NewOfferID()
		ids[i] = strconv.FormatInt(input.Bundles[i].ID, 10)
	}
	if err := s.store.ChangePlan(r.Context(), ownerID, int64(input.PlanID), subscription.PlanChange{Bundles: input.Bundles}); err != nil {
		subscriptionError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "result": map[string]any{"list": ids}})
}

func (s subscriptionAPI) deleteBundle(w http.ResponseWriter, r *http.Request, ownerID int64) {
	if !s.requireAnchor(w, r, ownerID) {
		return
	}
	var input struct {
		PlanID   subscriptionID `json:"plan_id"`
		BundleID subscriptionID `json:"buddle_id"`
	}
	if !decodeEmailRequest(w, r, &input) {
		return
	}
	if input.PlanID < 1 || input.BundleID < 1 {
		subscriptionError(w, subscription.ErrInvalidPlan)
		return
	}
	if err := s.store.ChangePlan(r.Context(), ownerID, int64(input.PlanID), subscription.PlanChange{RemoveBundleID: int64(input.BundleID)}); err != nil {
		subscriptionError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "result": struct{}{}})
}
