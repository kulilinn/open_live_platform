package httpapi

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"time"

	"open_live_platform/internal/payment"
	"open_live_platform/internal/subscription"
	"open_live_platform/internal/transport/httpx"
)

func (s subscriptionAPI) registerRenewal(mux *http.ServeMux, jobToken string) {
	renewals, hasStorage := s.store.(subscription.RenewalStore)
	mux.HandleFunc("POST /api/sub/renew_sub", func(w http.ResponseWriter, r *http.Request) {
		if jobToken == "" || !hasStorage || s.billing == nil {
			httpx.Error(w, http.StatusServiceUnavailable, "subscription renewal is unavailable")
			return
		}
		if !httpx.RequireToken(w, r, jobToken) {
			return
		}
		s.renewSubscriptions(w, r, renewals)
	})
}

type renewalResults struct {
	Renewed      int    `json:"renewed"`
	Declined     int    `json:"declined"`
	Skipped      int    `json:"skipped"`
	PendingOrder string `json:"pending_order,omitempty"`
}

func (s subscriptionAPI) renewSubscriptions(w http.ResponseWriter, r *http.Request, renewals subscription.RenewalStore) {
	ctx, stop := context.WithTimeout(r.Context(), 10*time.Second)
	defer stop()
	now := time.Now().UTC()
	cutoff := time.Date(now.Year(), now.Month(), now.Day()+2, 0, 0, 0, 0, time.UTC)
	candidates, err := renewals.RenewalCandidates(ctx, cutoff)
	if err != nil {
		httpx.Error(w, http.StatusServiceUnavailable, "could not find renewal candidates")
		return
	}
	var results renewalResults
	for _, candidate := range candidates {
		if ctx.Err() != nil {
			break
		}
		members, err := s.members(ctx, candidate.FanID, candidate.AnchorID)
		if errors.Is(err, subscription.ErrNotFound) || errors.Is(err, subscription.ErrForbidden) {
			results.Skipped++
			continue
		}
		if err != nil {
			httpx.Error(w, http.StatusServiceUnavailable, "could not verify renewal accounts")
			return
		}
		order, err := renewals.ReserveRenewal(ctx, candidate, "sub-"+rand.Text())
		if errors.Is(err, subscription.ErrRenewalIneligible) || errors.Is(err, subscription.ErrNotFound) || !order.New && err == nil {
			results.Skipped++
			continue
		}
		if err != nil {
			httpx.Error(w, http.StatusServiceUnavailable, "could not reserve subscription renewal")
			return
		}
		// Finish a reserved payment even if the scheduler disconnects.
		workCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		receipt, err := s.billing.Decrease(workCtx, candidate.FanID, order.Points, order.OrderNo)
		switch {
		case errors.Is(err, payment.ErrInsufficientPoints):
			if err := renewals.DeclineRenewal(workCtx, order.OrderNo, members); err == nil {
				results.Declined++
			} else {
				results.PendingOrder = order.OrderNo
			}
		case err != nil:
			results.PendingOrder = order.OrderNo
		default:
			if err := s.store.ConfirmPurchase(workCtx, order.OrderNo, members, subscriptionCharge(receipt)); err == nil {
				results.Renewed++
			} else {
				results.PendingOrder = order.OrderNo
			}
		}
		cancel()
		if results.PendingOrder != "" {
			break
		}
	}
	if results.PendingOrder != "" || ctx.Err() != nil {
		httpx.JSON(w, http.StatusServiceUnavailable, map[string]any{"error": "subscription renewal needs another check", "result": results})
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "result": results})
}

func subscriptionCharge(receipt payment.Receipt) subscription.Charge {
	return subscription.Charge{OrderID: receipt.OrderID, OrderNo: receipt.OrderNo, TotalPoints: receipt.TotalPoints, BadDebtPoints: receipt.BadDebt}
}
