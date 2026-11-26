package httpapi

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"strings"
	"time"

	"open_live_platform/internal/payment"
	"open_live_platform/internal/posts"
	"open_live_platform/internal/transport/httpx"
)

func (s postAPI) payPost(w http.ResponseWriter, r *http.Request, actor int64) {
	id, ok := postQueryID(w, r, "post_id")
	if !ok {
		return
	}
	s.chargePost(w, r, posts.Order{ActorID: actor, PostID: id, Kind: posts.Unlock})
}

func (s postAPI) donatePost(w http.ResponseWriter, r *http.Request, actor int64) {
	var input struct {
		PostID  subscriptionID `json:"post_id"`
		ActorID subscriptionID `json:"user_id"`
		Amount  int64          `json:"amount"`
	}
	if !decodePost(w, r, &input) {
		return
	}
	if int64(input.ActorID) != actor {
		postError(w, posts.ErrForbidden)
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if input.PostID < 1 || input.Amount < 1 || input.Amount > 1<<31-1 || len(key) > 128 {
		postError(w, posts.ErrInvalid)
		return
	}
	s.chargePost(w, r, posts.Order{ActorID: actor, PostID: int64(input.PostID), Kind: posts.Tip, Points: input.Amount, Key: key})
}

func (s postAPI) chargePost(w http.ResponseWriter, r *http.Request, request posts.Order) {
	store, durable := s.entries.(posts.OrderStore)
	if !durable || s.billing == nil {
		postError(w, posts.ErrPaymentUnavailable)
		return
	}
	request.Number = "anchor-post-" + rand.Text()
	order, err := store.ReservePostOrder(r.Context(), request)
	if err != nil {
		postError(w, err)
		return
	}
	success := func() {
		if order.Kind == posts.Tip {
			httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": nil})
			return
		}
		postStatus(w, nil)
	}
	if order.State == posts.OrderPaid {
		success()
		return
	}
	if order.State == posts.OrderDeclined {
		postInsufficientPoints(w)
		return
	}
	if !order.New {
		postPaymentPending(w, order.Number)
		return
	}

	// Once reserved, a disconnected client must not prevent receipt processing.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
	defer cancel()
	receipt, err := s.billing.Decrease(ctx, order.ActorID, order.Points, order.Number)
	if errors.Is(err, payment.ErrInsufficientPoints) {
		if err := store.CompletePostOrder(ctx, order, nil); err != nil {
			postPaymentPending(w, order.Number)
			return
		}
		postInsufficientPoints(w)
		return
	}
	if err != nil || store.CompletePostOrder(ctx, order, &receipt) != nil {
		postPaymentPending(w, order.Number)
		return
	}
	success()
}

func postInsufficientPoints(w http.ResponseWriter) {
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "4046", "message": "insufficient points"})
}

func postPaymentPending(w http.ResponseWriter, number string) {
	httpx.JSON(w, http.StatusServiceUnavailable, map[string]string{"error": "post payment awaits confirmation", "order_no": number})
}
