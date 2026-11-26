package httpapi

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"net/http"
	"time"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/date"
	"open_live_platform/internal/gift"
	"open_live_platform/internal/payment"
	"open_live_platform/internal/transport/httpx"
)

func (a giftInteractionsAPI) voteGame(w http.ResponseWriter, r *http.Request, actor int64) {
	var input struct {
		Channel  int64  `json:"dateRoomId,string"`
		GiftID   int64  `json:"giftId,string"`
		Audience int64  `json:"userId,string"`
		Anchor   int64  `json:"anchorId,string"`
		Kind     *uint8 `json:"gameType"`
		Role     uint8  `json:"playerType"`
		Accept   *bool  `json:"acceptFlag"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if !httpx.Decode(w, r, &input) {
		return
	}
	if input.Kind == nil || *input.Kind > 1 || input.Accept == nil {
		gameError(w, date.ErrGameMove)
		return
	}
	ctx := r.Context()
	session, err := a.dates.Service.SessionFor(ctx, input.Channel, actor, true)
	if err == nil && (session.AnchorID != input.Anchor || session.AudienceID != input.Audience ||
		gameRole(session, actor) != input.Role) {
		err = date.ErrForbidden
	}
	if err != nil {
		gameError(w, err)
		return
	}
	store, ok := a.dates.Service.Store.(date.GiftGameStore)
	orders, haveOrders := a.dates.Service.Store.(date.GiftStore)
	if !ok || !haveOrders {
		httpx.Error(w, http.StatusServiceUnavailable, "gift game storage unavailable")
		return
	}
	item, ok, err := a.catalog.Lookup(ctx, input.GiftID)
	if err != nil {
		gameError(w, err)
		return
	}
	if !ok || item.GiftType != 1 {
		gameError(w, gift.ErrOrderNotFound)
		return
	}
	candidate, err := a.gameCandidate(ctx, session, item)
	if err != nil {
		gameError(w, err)
		return
	}
	if *input.Accept && candidate.Cost > 0 && a.billing == nil {
		gameError(w, payment.ErrUnavailable)
		return
	}
	var questions []gift.Question
	if *input.Kind == 1 {
		questions = a.content.ActiveQuestions()
		if len(questions) >= 5 {
			rand.Shuffle(len(questions), func(i, j int) { questions[i], questions[j] = questions[j], questions[i] })
			questions = questions[:5]
		}
	}
	game, changed, err := store.VoteDateGiftGame(ctx, candidate, actor, *input.Kind, *input.Accept, questions)
	if err != nil {
		gameError(w, err)
		return
	}
	switch game.Stage {
	case date.GameRejected:
		if changed {
			a.pushGame(ctx, session, map[string]any{"status": "500", "data": map[string]any{
				"accept_flag": false, "reject_person": game.RejectedBy,
			}})
		}
	case date.GamePayment:
		a.payGame(w, r, session, game, store, orders)
		return
	}
	collectibleData(w, "success")
}

func (a giftInteractionsAPI) gameCandidate(ctx context.Context, session date.Session, item gift.Gift) (date.GiftOrder, error) {
	input := date.GiftInput{Channel: session.Channel, GiftID: item.ID, Sender: session.AudienceID,
		Receiver: session.AnchorID, Quantity: 1}
	cost, err := date.GiftCost(input, item)
	if err != nil {
		return date.GiftOrder{}, err
	}
	if err := auth.CheckCommunication(ctx, a.dates.Accounts, input.Sender); err != nil {
		return date.GiftOrder{}, err
	}
	sender, _, err := a.dates.Accounts.Find(ctx, input.Sender)
	if err != nil {
		return date.GiftOrder{}, err
	}
	recipient, _, err := a.dates.Accounts.Find(ctx, input.Receiver)
	if err != nil {
		return date.GiftOrder{}, err
	}
	return date.GiftOrder{ID: date.NewChannel(), Input: input, Key: "game-" + cryptorand.Text(),
		PaymentNo: "date-gift-game-" + cryptorand.Text(), Cost: cost, Income: cost,
		Item: item, Sender: giftUser(sender), Receiver: giftUser(recipient)}, nil
}

func (a giftInteractionsAPI) payGame(w http.ResponseWriter, r *http.Request, session date.Session, game date.GiftGame,
	games date.GiftGameStore, orders date.GiftStore) {
	ctx := r.Context()
	// DecideDateGift acquires the channel lock and gives only one caller
	// permission to submit the external point debit.
	order, shouldPay, err := orders.DecideDateGift(ctx, session.Channel, game.OrderID, session.AnchorID, true)
	if err != nil {
		gameError(w, err)
		return
	}
	w.Header().Set("X-Payment-Order", order.PaymentNo)
	if shouldPay {
		payments := dateGiftsAPI{dates: a.dates, store: orders, billing: a.billing, catalog: a.catalog}
		order, err = payments.settleDateGift(ctx, order)
		if err != nil {
			gameError(w, err)
			return
		}
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	started, changed, err := games.BeginPaidGiftGame(ctx, session.Channel, game.ID)
	if err != nil {
		gameError(w, err)
		return
	}
	if started.Stage == date.GameRejected && order.State == date.GiftDeclined {
		gameError(w, payment.ErrInsufficientPoints)
		return
	}
	if changed && started.Stage == date.GamePlaying {
		payload := ""
		if game.Kind == 1 {
			data, err := json.Marshal(started.Questions)
			if err != nil {
				gameError(w, err)
				return
			}
			payload = string(data)
		}
		a.pushGame(ctx, session, map[string]any{"status": "200", "data": payload})
	}
	collectibleData(w, "success")
}

func (a giftInteractionsAPI) moveGame(kind uint8) userHandler {
	return func(w http.ResponseWriter, r *http.Request, actor int64) {
		var input struct {
			Channel int64 `json:"dateRoomId,string"`
			Player  int64 `json:"playerId,string"`
			Role    uint8 `json:"playerType"`
			Move    struct {
				Quan  *uint8 `json:"quan"`
				Guess *struct {
					ID     int64  `json:"questionId,string"`
					Answer string `json:"answer"`
				} `json:"guess"`
			} `json:"gameType"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
		if !httpx.Decode(w, r, &input) {
			return
		}
		ctx := r.Context()
		session, err := a.dates.Service.SessionFor(ctx, input.Channel, actor, true)
		if err == nil && (input.Player != actor || gameRole(session, actor) != input.Role) {
			err = date.ErrForbidden
		}
		if err != nil {
			gameError(w, err)
			return
		}
		store, ok := a.dates.Service.Store.(date.GiftGameStore)
		if !ok {
			httpx.Error(w, http.StatusServiceUnavailable, "gift game storage unavailable")
			return
		}
		move := date.GameMove{Kind: kind}
		switch {
		case kind == 0 && input.Move.Quan != nil && input.Move.Guess == nil:
			move.Gesture = *input.Move.Quan
		case kind == 1 && input.Move.Guess != nil && input.Move.Quan == nil:
			move.Question, move.Answer = input.Move.Guess.ID, input.Move.Guess.Answer
		default:
			gameError(w, date.ErrGameMove)
			return
		}
		result, err := store.MoveGiftGame(ctx, input.Channel, actor, move)
		if err != nil {
			gameError(w, err)
			return
		}
		if result != nil {
			a.pushGame(ctx, session, result)
		}
		collectibleData(w, "success")
	}
}

func (a giftInteractionsAPI) pushGame(ctx context.Context, session date.Session, payload any) {
	a.dates.push(ctx, session.AnchorID, "gift_game", payload)
	a.dates.push(ctx, session.AudienceID, "gift_game", payload)
}

func gameRole(session date.Session, userID int64) uint8 {
	if session.AnchorID == userID {
		return 1
	}
	return 2
}

func gameError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, date.ErrGameBusy), errors.Is(err, date.ErrGameNotReady):
		writeGiftError(w, http.StatusOK, "4029", err.Error())
	case errors.Is(err, date.ErrGameMove):
		httpx.Error(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, date.ErrGameQuestions):
		httpx.Error(w, http.StatusServiceUnavailable, err.Error())
	default:
		dateGiftResult(w, nil, err)
	}
}
