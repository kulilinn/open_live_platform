package httpapi

import (
	"context"
	"crypto/rand"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/gift"
	"open_live_platform/internal/live"
	"open_live_platform/internal/payment"
	"open_live_platform/internal/transport/httpx"
)

func RegisterGiftOrders(mux *http.ServeMux, accounts auth.AccountStore, rooms live.RoomStore, store gift.OrderStore, billing *payment.Client, catalog *gift.Catalog) {
	s := server{accounts: accounts, rooms: rooms}
	_, effectsAvailable := store.(gift.RoomEffectsStore)
	give := func(rocketOnly bool) http.HandlerFunc {
		return s.authenticated(func(w http.ResponseWriter, r *http.Request, userID int64) {
			var input gift.Input
			if !httpx.Decode(w, r, &input) {
				return
			}
			roomID, parseErr := strconv.ParseInt(input.Room, 10, 64)
			if parseErr != nil || roomID <= 0 || input.LiveID <= 0 || input.GiftID <= 0 || input.ReceiverID <= 0 || input.Quantity <= 0 {
				writeGiftError(w, http.StatusBadRequest, "400", "invalid gift purchase")
				return
			}
			if input.SenderID != userID {
				writeGiftError(w, http.StatusUnauthorized, "401", "Unauthorized request")
				return
			}
			input.Room = strconv.FormatInt(roomID, 10)
			key := r.Header.Get("Idempotency-Key")
			if len(key) > 128 || strings.TrimSpace(key) != key || strings.ContainsAny(key, " \t") {
				writeGiftError(w, http.StatusBadRequest, "400", "invalid Idempotency-Key")
				return
			}
			if key == "" {
				key = rand.Text()
			}
			w.Header().Set("Idempotency-Key", key)
			order, err := store.FindGiftOrder(r.Context(), userID, key)
			if err == nil {
				if order.Input != input || (rocketOnly && order.Item.EffectControlType != gift.RocketEffect) {
					giftOrderResponse(w, gift.Order{}, gift.ErrOrderConflict)
					return
				}
				giftOrderResponse(w, order, nil)
				return
			}
			if !errors.Is(err, gift.ErrOrderNotFound) {
				giftOrderResponse(w, gift.Order{}, err)
				return
			}
			item, exists, err := catalog.Lookup(r.Context(), input.GiftID)
			if err != nil {
				giftOrderResponse(w, gift.Order{}, err)
				return
			}
			if !exists {
				writeGiftError(w, http.StatusOK, "4005", "gift:"+strconv.FormatInt(input.GiftID, 10)+" not found")
				return
			}
			if (rocketOnly && item.EffectControlType != gift.RocketEffect) ||
				((item.QueuesEffect() || item.EffectControlType == gift.RefuelingEffect) && !effectsAvailable) {
				giftOrderResponse(w, gift.Order{}, gift.ErrUnsupported)
				return
			}
			cost, income, err := item.Cost(input.Quantity, input.LimitID)
			if err != nil {
				giftOrderResponse(w, gift.Order{}, err)
				return
			}
			if cost > 0 && billing == nil {
				giftOrderResponse(w, gift.Order{}, payment.ErrUnavailable)
				return
			}
			sender, senderOK, err := accounts.Find(r.Context(), userID)
			if err != nil || !senderOK {
				writeGiftError(w, http.StatusInternalServerError, "500", "could not read gift sender")
				return
			}
			receiver, receiverOK, err := accounts.Find(r.Context(), input.ReceiverID)
			if err != nil {
				giftOrderResponse(w, gift.Order{}, err)
				return
			}
			if !receiverOK {
				writeGiftError(w, http.StatusOK, "4005", "data not found ")
				return
			}
			order, created, err := store.ReserveGift(r.Context(), roomID, gift.Order{
				Input: input, Key: key, PaymentOrder: "gift-" + rand.Text(), Item: item, Cost: cost, Income: income,
				Sender: giftUser(sender), Receiver: giftUser(receiver), SenderRole: 2,
			})
			if err != nil || !created {
				giftOrderResponse(w, order, err)
				return
			}
			var receipt payment.Receipt
			if cost > 0 {
				receipt, err = billing.Decrease(r.Context(), userID, cost, order.PaymentOrder)
			}
			declined := errors.Is(err, payment.ErrInsufficientPoints)
			if err != nil && !declined {
				giftOrderResponse(w, order, gift.ErrPending)
				return
			}
			// Once the provider answers, record the decision even if the caller went away.
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
			defer cancel()
			completed, err := store.FinishGift(ctx, roomID, order, receipt, declined)
			if err != nil && !declined && cost > 0 {
				giftOrderResponse(w, order, gift.ErrPending)
				return
			}
			giftOrderResponse(w, completed, err)
		})
	}
	mux.HandleFunc("POST /api/date/1vN/give/gifts", give(false))
	mux.HandleFunc("POST /api/date/1vN/give/rocket", give(true))
	mux.HandleFunc("GET /api/live/room/rank/{room}", s.authenticated(s.roomGiftRank(store, false)))
	mux.HandleFunc("POST /api/live/room/rank/{room}", s.authenticated(s.roomGiftRank(store, true)))
	mux.HandleFunc("GET /api/date/anchor/income/log", s.authenticated(s.giftEarnings(store, false)))
	mux.HandleFunc("GET /api/date/anchor/total/income", s.authenticated(s.giftEarnings(store, true)))
}

func giftUser(user auth.User) gift.User {
	result := gift.User{ID: user.ID, Name: user.Account, SearchName: user.Account}
	if user.Profile != nil {
		result.Photo, result.SearchName = user.Profile.Photo, user.Profile.SearchName
	}
	return result
}

func (s server) giftEarnings(store gift.OrderStore, totalOnly bool) userHandler {
	return func(w http.ResponseWriter, r *http.Request, userID int64) {
		roomID, roomErr := strconv.ParseInt(r.URL.Query().Get("room_id"), 10, 64)
		anchorID, anchorErr := strconv.ParseInt(r.URL.Query().Get("anchor_id"), 10, 64)
		if roomErr != nil || anchorErr != nil || roomID < 1 || anchorID < 1 {
			writeGiftError(w, http.StatusBadRequest, "400", "invalid room or anchor")
			return
		}
		if anchorID != userID {
			giftOrderResponse(w, gift.Order{}, live.ErrForbidden)
			return
		}
		room, err := s.rooms.Info(r.Context(), roomID)
		if err != nil {
			giftOrderResponse(w, gift.Order{}, err)
			return
		}
		if room.OwnerID != anchorID {
			giftOrderResponse(w, gift.Order{}, live.ErrForbidden)
			return
		}
		if totalOnly {
			income, err := store.GiftTotalIncome(r.Context(), roomID)
			if err != nil {
				giftOrderResponse(w, gift.Order{}, err)
				return
			}
			var value any
			if income != nil {
				value = strconv.FormatInt(*income, 10)
			}
			httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": value})
			return
		}
		items, err := store.GiftIncome(r.Context(), roomID, room.Session.LiveID)
		if err != nil {
			giftOrderResponse(w, gift.Order{}, err)
			return
		}
		var sum int64
		for _, item := range items {
			if sum > math.MaxInt64-item.Points {
				writeGiftError(w, http.StatusInternalServerError, "500", "gift income exceeds supported range")
				return
			}
			sum += item.Points
		}
		yen, err := gift.Yen(sum)
		if err != nil {
			writeGiftError(w, http.StatusInternalServerError, "500", "gift income exceeds supported range")
			return
		}
		httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": map[string]any{"list": items, "sum": sum, "yen": yen}})
	}
}

func (s server) roomGiftRank(store gift.OrderStore, paged bool) userHandler {
	return func(w http.ResponseWriter, r *http.Request, _ int64) {
		roomID, ok := httpx.PathID(w, r, "room")
		if !ok {
			return
		}
		room, err := s.rooms.Info(r.Context(), roomID)
		if err != nil {
			roomStatusError(w, err)
			return
		}
		ranks, err := store.GiftRank(r.Context(), room.OwnerID, time.Now().UTC().Truncate(time.Hour).Add(-24*time.Hour))
		if err != nil {
			giftOrderResponse(w, gift.Order{}, err)
			return
		}
		response := map[string]any{"status": "200"}
		if paged {
			input := struct {
				Page uint64
				Size uint64 `json:"pageSize"`
			}{Size: 10}
			if !httpx.Decode(w, r, &input) {
				return
			}
			if input.Size == 0 {
				input.Size = 10
			}
			count := uint64(len(ranks))
			pages := count / input.Size
			if count%input.Size != 0 {
				pages++
			}
			response["page"], response["pages"], response["members"] = input.Page, pages, count
			if input.Page >= pages {
				ranks = ranks[:0]
			} else {
				start := input.Page * input.Size
				ranks = ranks[start : start+min(input.Size, count-start)]
			}
		}
		ids := make([]int64, len(ranks))
		var spend int64
		for i, ranking := range ranks {
			ids[i] = ranking.UserID
			if spend > math.MaxInt64-ranking.Income {
				writeGiftError(w, http.StatusInternalServerError, "500", "gift rank exceeds supported range")
				return
			}
			spend += ranking.Income
		}
		admins, err := s.rooms.Admins(r.Context(), roomID)
		if err != nil {
			roomStatusError(w, err)
			return
		}
		muted, err := s.rooms.MutedUsers(r.Context(), roomID)
		if err != nil {
			roomStatusError(w, err)
			return
		}
		data, err := s.audienceData(r.Context(), ids, admins, muted)
		if err != nil {
			giftOrderResponse(w, gift.Order{}, err)
			return
		}
		for i := range data {
			data[i]["spend"] = ranks[i].Income
		}
		response["data"], response["rankSpend"] = data, spend
		httpx.JSON(w, http.StatusOK, response)
	}
}

func giftOrderResponse(w http.ResponseWriter, order gift.Order, err error) {
	if order.PaymentOrder != "" {
		w.Header().Set("X-Payment-Order", order.PaymentOrder)
	}
	switch {
	case errors.Is(err, live.ErrRoomNotFound):
		roomStatusError(w, err)
	case errors.Is(err, live.ErrForbidden), errors.Is(err, live.ErrNotMember):
		writeGiftError(w, http.StatusForbidden, "403", "Operation not allowed")
	case errors.Is(err, live.ErrRoomClosed):
		writeGiftError(w, http.StatusConflict, "409", "broadcast is no longer active")
	case errors.Is(err, gift.ErrOrderConflict), errors.Is(err, gift.ErrCatalogConflict):
		writeGiftError(w, http.StatusConflict, "409", err.Error())
	case errors.Is(err, gift.ErrEffectBusy):
		writeGiftError(w, http.StatusConflict, "409", err.Error())
	case errors.Is(err, gift.ErrRocketUnavailable):
		writeGiftError(w, http.StatusServiceUnavailable, "503", err.Error())
	case errors.Is(err, gift.ErrCannotRefuel):
		writeGiftError(w, http.StatusOK, "4086", err.Error())
	case errors.Is(err, gift.ErrUnsupported):
		writeGiftError(w, http.StatusUnprocessableEntity, "422", err.Error())
	case errors.Is(err, gift.ErrPending):
		writeGiftError(w, http.StatusConflict, "409", "gift payment requires reconciliation")
	case errors.Is(err, payment.ErrUnavailable):
		writeGiftError(w, http.StatusServiceUnavailable, "503", "payment provider unavailable")
	case err != nil:
		writeGiftError(w, http.StatusInternalServerError, "500", "Internal error")
	case order.State == gift.Declined:
		writeGiftError(w, http.StatusOK, "4046", "have not enough points")
	case order.State == gift.Paid:
		httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": order.Result})
	default:
		writeGiftError(w, http.StatusConflict, "409", "gift payment requires reconciliation")
	}
}

func writeGiftError(w http.ResponseWriter, code int, status, message string) {
	httpx.JSON(w, code, map[string]string{"status": status, "message": message})
}
