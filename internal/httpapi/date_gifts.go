package httpapi

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/date"
	"open_live_platform/internal/gift"
	"open_live_platform/internal/payment"
	"open_live_platform/internal/transport/httpx"
)

type dateGiftsAPI struct {
	dates   *Dates
	store   date.GiftStore
	billing *payment.Client
	catalog []gift.Gift
}

func RegisterDateGiftOrders(mux *http.ServeMux, dates *Dates, billing *payment.Client, catalog []gift.Gift) {
	store, ok := dates.Service.Store.(date.GiftStore)
	if !ok {
		return
	}
	s := server{accounts: dates.Accounts}
	a := dateGiftsAPI{dates, store, billing, catalog}
	mux.HandleFunc("POST /api/date/give/gifts", s.authenticated(a.give))
	mux.HandleFunc("GET /api/date/gifts/{channel}", s.authenticated(a.read("catalog")))
	mux.HandleFunc("GET /api/date/gifts/transaction/{channel}", s.authenticated(a.read("transactions")))
	mux.HandleFunc("GET /api/date/gift/executing/{channel}", s.authenticated(a.read("current")))
	mux.HandleFunc("GET /api/date/gift/list/executing/{channel}", s.authenticated(a.read("queue")))
	mux.HandleFunc("GET /api/gift/requests/{channel}", s.authenticated(a.read("requests")))
	mux.HandleFunc("POST /api/gift/accept", s.authenticated(a.accept))
}

func (a *Dates) SweepGiftQueue(ctx context.Context) error {
	store, ok := a.Service.Store.(date.GiftStore)
	if !ok {
		return nil
	}
	started, err := store.ClaimDateGiftStarts(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	for _, entry := range started {
		remaining := max(0, int64((entry.End.Sub(now)+time.Second-1)/time.Second))
		if remaining == 0 {
			continue
		}
		payload := date.GiftView(entry.Order, entry.ID, remaining, entry.End)
		a.push(ctx, entry.Order.Input.Receiver, "give_gifts_toy", payload)
		a.push(ctx, entry.Order.Input.Sender, "give_gifts_toy", payload)
	}
	return nil
}

func dateGiftResult(w http.ResponseWriter, data any, err error) {
	switch {
	case errors.Is(err, gift.ErrOrderNotFound):
		dateResponse(w, date.ErrNotFound, nil)
	case errors.Is(err, gift.ErrBlocked):
		dateResponse(w, date.ErrForbidden, nil)
	case errors.Is(err, date.ErrGameWaiting):
		writeGiftError(w, http.StatusOK, "4028", err.Error())
	case errors.Is(err, payment.ErrInsufficientPoints):
		writeGiftError(w, http.StatusOK, "4027", err.Error())
	case errors.Is(err, gift.ErrPending), errors.Is(err, gift.ErrOrderConflict), errors.Is(err, gift.ErrUnsupported), errors.Is(err, payment.ErrUnavailable):
		giftOrderResponse(w, gift.Order{}, err)
	default:
		dateResponse(w, err, map[string]any{"data": data})
	}
}

func dateGiftOrderResponse(w http.ResponseWriter, order date.GiftOrder) {
	if order.PaymentNo != "" {
		w.Header().Set("X-Payment-Order", order.PaymentNo)
	}
	var err error
	switch order.State {
	case date.GiftPending:
		err = gift.ErrPending
	case date.GiftDeclined:
		err = payment.ErrInsufficientPoints
	}
	dateGiftResult(w, "success", err)
}

func (a dateGiftsAPI) give(w http.ResponseWriter, r *http.Request, actor int64) {
	var input date.GiftInput
	if !httpx.Decode(w, r, &input) {
		return
	}
	if actor != input.Sender {
		dateGiftResult(w, nil, date.ErrForbidden)
		return
	}
	ctx := r.Context()
	session, err := a.dates.Service.SessionFor(ctx, input.Channel, actor, false)
	if err != nil || session.AudienceID != actor || session.AnchorID != input.Receiver {
		if err == nil {
			err = date.ErrForbidden
		}
		dateGiftResult(w, nil, err)
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) > 128 || strings.TrimSpace(key) != key || strings.ContainsAny(key, " \t") {
		writeGiftError(w, http.StatusBadRequest, "400", "invalid Idempotency-Key")
		return
	}
	if key == "" {
		key = rand.Text()
	}
	w.Header().Set("Idempotency-Key", key)
	order, err := a.store.FindDateGift(ctx, actor, key)
	if err == nil {
		if order.Input != input {
			dateGiftResult(w, nil, gift.ErrOrderConflict)
			return
		}
		dateGiftOrderResponse(w, order)
		return
	}
	if !errors.Is(err, gift.ErrOrderNotFound) {
		dateGiftResult(w, nil, err)
		return
	}
	if !session.Active(time.Now()) {
		dateGiftResult(w, nil, date.ErrClosed)
		return
	}
	if err := auth.CheckCommunication(ctx, a.dates.Accounts, actor); err != nil {
		dateGiftResult(w, nil, err)
		return
	}
	var item *gift.Gift
	for i := range a.catalog {
		if a.catalog[i].ID == input.GiftID {
			item = &a.catalog[i]
			break
		}
	}
	if item == nil {
		dateGiftResult(w, nil, gift.ErrOrderNotFound)
		return
	}
	cost, err := date.GiftCost(input, *item)
	if err != nil || cost > 0 && a.billing == nil {
		if err == nil {
			err = payment.ErrUnavailable
		}
		dateGiftResult(w, nil, err)
		return
	}
	audience, _, err := a.dates.Accounts.Find(ctx, actor)
	if err != nil {
		dateGiftResult(w, nil, err)
		return
	}
	anchor, _, err := a.dates.Accounts.Find(ctx, input.Receiver)
	if err != nil {
		dateGiftResult(w, nil, err)
		return
	}
	state := date.GiftPending
	if item.GiftType == 1 {
		state = date.GiftWaiting
		if cost > 0 {
			balance, err := a.billing.Balance(ctx, actor)
			if err == nil && balance < cost {
				err = payment.ErrInsufficientPoints
			}
			if err != nil {
				dateGiftResult(w, nil, err)
				return
			}
		}
	}
	order, created, err := a.store.ReserveDateGift(ctx, date.GiftOrder{
		ID: date.NewChannel(), Input: input, Cost: cost, Income: cost, State: state, Item: *item,
		Key: key, PaymentNo: "date-gift-" + rand.Text(), Sender: giftUser(audience), Receiver: giftUser(anchor),
	})
	if err != nil {
		dateGiftResult(w, nil, err)
		return
	}
	if !created {
		dateGiftOrderResponse(w, order)
		return
	}
	if state == date.GiftWaiting {
		a.dates.push(ctx, order.Input.Receiver, "request_gifts_game", dateGiftRequest(order))
		dateGiftOrderResponse(w, order)
		return
	}
	a.debit(w, r, order)
}

func (a dateGiftsAPI) debit(w http.ResponseWriter, r *http.Request, order date.GiftOrder) {
	w.Header().Set("X-Payment-Order", order.PaymentNo)
	completed, err := a.settleDateGift(r.Context(), order)
	if err != nil {
		dateGiftResult(w, nil, err)
		return
	}
	dateGiftOrderResponse(w, completed)
}

func (a dateGiftsAPI) settleDateGift(ctx context.Context, order date.GiftOrder) (date.GiftOrder, error) {
	var receipt payment.Receipt
	var err error
	if order.Cost > 0 {
		receipt, err = a.billing.Decrease(ctx, order.Input.Sender, order.Cost, order.PaymentNo)
	}
	declined := errors.Is(err, payment.ErrInsufficientPoints)
	if err != nil && !declined {
		return date.GiftOrder{}, gift.ErrPending
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	completed, changed, err := a.store.FinishDateGift(ctx, order, receipt, declined)
	if err != nil {
		return date.GiftOrder{}, gift.ErrPending
	}
	if changed && completed.State == date.GiftPaid {
		a.notifyPaid(ctx, completed)
	}
	return completed, nil
}

func (a dateGiftsAPI) notifyPaid(ctx context.Context, o date.GiftOrder) {
	for _, user := range []int64{o.Input.Receiver, o.Input.Sender} {
		switch o.Item.GiftType {
		case 0:
			a.dates.push(ctx, user, "give_gifts_ordinary", map[string]any{
				"gift": date.GiftView(o, o.Item.ID, o.Duration(), time.Time{}), "quantity": o.Input.Quantity,
			})
		case 1:
			if user == o.Input.Sender {
				a.dates.push(ctx, user, "accept_gifts_game", dateGiftRequest(o))
			}
		case 2:
			if queue, err := a.queue(ctx, o.Input.Channel); err == nil && len(queue) > 0 {
				a.dates.push(ctx, user, "gift_list", queue)
			}
		}
		if history, err := a.transactions(ctx, o.Input.Channel); err == nil {
			a.dates.push(ctx, user, "gift_record_list", history)
		}
	}
}

func (a dateGiftsAPI) accept(w http.ResponseWriter, r *http.Request, actor int64) {
	var input struct {
		Channel int64 `json:"channel,string"`
		Gift    int64 `json:"gift,string"`
		Accept  bool  `json:"accept"`
	}
	if !httpx.Decode(w, r, &input) {
		return
	}
	order, changed, err := a.store.DecideDateGift(r.Context(), input.Channel, input.Gift, actor, input.Accept)
	if err != nil {
		dateGiftResult(w, nil, err)
		return
	}
	if changed && input.Accept {
		a.debit(w, r, order)
		return
	}
	if changed {
		a.dates.push(r.Context(), order.Input.Sender, "refuse_gifts_game", dateGiftRequest(order))
	}
	dateGiftOrderResponse(w, order)
}

func dateGiftRequest(o date.GiftOrder) []any {
	return []any{strconv.FormatInt(o.ID, 10), map[string]any{
		"dateRoomId": strconv.FormatInt(o.Input.Channel, 10), "giftId": strconv.FormatInt(o.Item.ID, 10),
		"senderId": strconv.FormatInt(o.Input.Sender, 10), "receiverId": strconv.FormatInt(o.Input.Receiver, 10),
		"name": o.Item.Name, "unitPrice": o.Item.Price, "quantity": o.Input.Quantity,
	}}
}

func (a dateGiftsAPI) read(kind string) userHandler {
	return func(w http.ResponseWriter, r *http.Request, actor int64) {
		channel, ok := dateID(w, r, "channel", 0)
		if !ok {
			return
		}
		ctx := r.Context()
		session, err := a.dates.Service.SessionFor(ctx, channel, actor, false)
		if err != nil {
			dateGiftResult(w, nil, err)
			return
		}
		switch kind {
		case "catalog":
			a.giftCatalog(ctx, w, channel)
		case "transactions":
			records, err := a.transactions(ctx, channel)
			dateGiftResult(w, records, err)
		case "requests":
			list := make([][]any, 0)
			if session.Active(time.Now()) {
				orders, err := a.store.DateGifts(ctx, channel, date.GiftWaiting)
				if err != nil {
					dateGiftResult(w, nil, err)
					return
				}
				for _, order := range orders {
					list = append(list, dateGiftRequest(order))
				}
			}
			dateGiftResult(w, list, nil)
		case "current", "queue":
			items := make([]map[string]any, 0)
			if session.Active(time.Now()) {
				items, err = a.queue(ctx, channel)
			}
			if kind == "current" {
				var current any
				if len(items) > 0 {
					current = items[0]
				}
				dateGiftResult(w, current, err)
			} else {
				dateGiftResult(w, items, err)
			}
		}
	}
}

func (a dateGiftsAPI) giftCatalog(ctx context.Context, w http.ResponseWriter, channel int64) {
	var blocked map[int64]struct{}
	if blockStore, ok := a.store.(gift.BlockStore); ok {
		var err error
		blocked, err = blockStore.BlockedDateGifts(ctx, channel)
		if err != nil {
			dateGiftResult(w, nil, err)
			return
		}
	}
	groups := make([][]gift.Gift, 4)
	for _, item := range a.catalog {
		if _, excluded := blocked[item.ID]; excluded {
			continue
		}
		group := int(item.GiftType)
		if group < 0 || group >= len(groups) {
			group = 3
		}
		groups[group] = append(groups[group], item)
	}
	names := []string{"普通礼物", "游戏礼物", "玩具礼物", "自定义礼物"}
	data := make([]map[string]any, 0, len(groups))
	for i, items := range groups {
		if len(items) != 0 {
			data = append(data, map[string]any{"type": i, "name": names[i], "data": items})
		}
	}
	dateGiftResult(w, data, nil)
}

func (a dateGiftsAPI) transactions(ctx context.Context, channel int64) ([]map[string]any, error) {
	list, err := a.store.DateGifts(ctx, channel, date.GiftPaid)
	if err != nil {
		return nil, err
	}
	records := make([]map[string]any, 0, len(list))
	for _, item := range list {
		records = append(records, map[string]any{
			"usersInfo": map[string]any{"receiverId": item.Receiver.ID, "receiverName": item.Receiver.Name,
				"sendId": item.Sender.ID, "sendName": item.Sender.Name},
			"giftInfo": date.GiftView(item, item.Item.ID, item.Duration(), time.Time{}),
			"quantity": item.Input.Quantity, "timestamp": item.PaidAt.Unix(),
		})
	}
	return records, nil
}

func (a dateGiftsAPI) queue(ctx context.Context, channel int64) ([]map[string]any, error) {
	queue, err := a.store.DateGiftQueue(ctx, channel)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	items := make([]map[string]any, 0, len(queue))
	for _, item := range queue {
		seconds := item.Order.Duration()
		if !now.Before(item.Start) {
			seconds = max(0, int64((item.End.Sub(now)+time.Second-1)/time.Second))
		}
		items = append(items, date.GiftView(item.Order, item.ID, seconds, item.End))
	}
	return items, nil
}
