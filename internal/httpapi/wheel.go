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
	"unicode/utf8"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/date"
	"open_live_platform/internal/gift"
	"open_live_platform/internal/live"
	"open_live_platform/internal/payment"
	"open_live_platform/internal/transport/httpx"
)

type wheelAPI struct {
	server
	store   gift.WheelStore
	billing *payment.Client
}

func RegisterWheel(mux *http.ServeMux, accounts auth.AccountStore, rooms live.RoomStore, billing *payment.Client) {
	store, ok := rooms.(gift.WheelStore)
	if !ok {
		return
	}
	a := wheelAPI{server{accounts: accounts, rooms: rooms}, store, billing}
	mux.HandleFunc("POST /api/date/set/wheel/price", a.authenticated(a.change("price")))
	mux.HandleFunc("POST /api/date/set/wheel/value", a.authenticated(a.change("values")))
	mux.HandleFunc("POST /api/date/wheel/open_or_close", a.authenticated(a.change("status")))
	mux.HandleFunc("GET /api/date/get/wheel/config/{anchor_id}", a.authenticated(a.config(true)))
	mux.HandleFunc("GET /api/date/get/wheel/value/{anchor_id}", a.authenticated(a.config(false)))
	mux.HandleFunc("GET /api/date/get/wheel/recommend/value", a.authenticated(a.recommendations))
	mux.HandleFunc("POST /api/date/play_wheel", a.authenticated(a.play))
	mux.HandleFunc("GET /api/date/get/wheel/result", a.authenticated(a.history))
}

func (a wheelAPI) change(kind string) userHandler {
	return func(w http.ResponseWriter, r *http.Request, actor int64) {
		var input struct {
			Anchor int64    `json:"anchor_id,string"`
			Price  int64    `json:"price"`
			Value  []string `json:"value"`
			Status *int16   `json:"open_status"`
		}
		if !httpx.Decode(w, r, &input) {
			return
		}
		if actor != input.Anchor {
			httpx.LiveError(w, live.ErrForbidden)
			return
		}
		user, ok, err := a.accounts.Find(r.Context(), actor)
		if err != nil {
			httpx.LiveError(w, err)
			return
		}
		if !ok || !auth.PublicProfile(user).IsAnchor {
			room, lookup := a.rooms.Info(r.Context(), actor)
			if lookup != nil || room.OwnerID != actor {
				httpx.LiveError(w, live.ErrForbidden)
				return
			}
		}
		var change gift.WheelChange
		switch kind {
		case "price":
			if input.Price < 10 || input.Price > math.MaxInt32 {
				httpx.Error(w, http.StatusBadRequest, "invalid wheel price")
				return
			}
			change.Price = &input.Price
		case "values":
			if len(input.Value) > 32 {
				httpx.Error(w, http.StatusBadRequest, "too many wheel options")
				return
			}
			options := make([]string, 0, len(input.Value))
			for _, value := range input.Value {
				if strings.TrimSpace(value) == "" || strings.ContainsRune(value, 0) || utf8.RuneCountInString(value) > 160 {
					httpx.Error(w, http.StatusBadRequest, "invalid wheel option")
					return
				}
				options = append(options, value)
			}
			change.Options = &options
		case "status":
			if input.Status == nil || *input.Status < 0 || *input.Status > 1 {
				httpx.Error(w, http.StatusBadRequest, "invalid wheel status")
				return
			}
			change.Status = input.Status
		}
		if err := a.store.UpdateWheelConfig(r.Context(), actor, change); err != nil {
			httpx.LiveError(w, err)
			return
		}
		dateGiftResult(w, "", nil)
	}
}

func (a wheelAPI) config(full bool) userHandler {
	return func(w http.ResponseWriter, r *http.Request, _ int64) {
		anchor, ok := httpx.PathID(w, r, "anchor_id")
		if !ok {
			return
		}
		config, err := a.store.WheelConfig(r.Context(), anchor)
		if err != nil {
			httpx.LiveError(w, err)
			return
		}
		if full {
			dateGiftResult(w, config, nil)
			return
		}
		httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": config.Options, "open_status": config.OpenStatus})
	}
}

func (a wheelAPI) recommendations(w http.ResponseWriter, r *http.Request, _ int64) {
	options, err := a.store.WheelRecommendations(r.Context())
	dateGiftResult(w, options, err)
}

func (a wheelAPI) play(w http.ResponseWriter, r *http.Request, actor int64) {
	var input struct {
		Anchor   int64  `json:"anchorId,string"`
		User     int64  `json:"userId,string"`
		Live     int64  `json:"liveId,string"`
		Room     string `json:"dateRoomId"`
		ChatRoom string `json:"chatRoomId"`
	}
	if !httpx.Decode(w, r, &input) {
		return
	}
	room, parseErr := strconv.ParseInt(input.Room, 10, 64)
	if parseErr != nil || input.Anchor <= 0 || input.Live <= 0 || room <= 0 ||
		(input.ChatRoom != "" && input.ChatRoom != strconv.FormatInt(room, 10)) {
		httpx.Error(w, http.StatusBadRequest, "invalid wheel room")
		return
	}
	if actor != input.User || actor == input.Anchor || room != input.Anchor {
		httpx.LiveError(w, live.ErrForbidden)
		return
	}
	ctx := r.Context()
	if err := auth.CheckCommunication(ctx, a.accounts, actor); err != nil {
		dateGiftResult(w, nil, err)
		return
	}
	if a.billing == nil {
		dateGiftResult(w, nil, payment.ErrUnavailable)
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) > 128 || strings.TrimSpace(key) != key || strings.ContainsAny(key, " \t") {
		httpx.Error(w, http.StatusBadRequest, "invalid Idempotency-Key")
		return
	}
	if key == "" {
		key = rand.Text()
	}
	w.Header().Set("Idempotency-Key", key)
	user, ok, err := a.accounts.Find(ctx, actor)
	if err != nil || !ok {
		dateGiftResult(w, nil, date.ErrForbidden)
		return
	}
	order, created, err := a.store.ReserveWheel(ctx, gift.WheelOrder{
		ID: date.NewChannel(), Key: key, PaymentNo: "wheel-" + rand.Text(),
		WheelInput: gift.WheelInput{RoomID: room, LiveID: input.Live, UserID: actor, AnchorID: input.Anchor},
		Result:     gift.WheelResult{UserID: actor, UserRole: 2, UserName: user.Account, UserAvatar: auth.PublicProfile(user).Photo},
	})
	if err != nil {
		if errors.Is(err, gift.ErrWheelClosed) {
			httpx.Error(w, http.StatusConflict, err.Error())
		} else {
			giftOrderResponse(w, gift.Order{}, err)
		}
		return
	}
	w.Header().Set("X-Payment-Order", order.PaymentNo)
	if created {
		receipt, err := a.billing.Decrease(ctx, actor, order.Price, order.PaymentNo)
		declined := errors.Is(err, payment.ErrInsufficientPoints)
		if err != nil && !declined {
			dateGiftResult(w, nil, gift.ErrPending)
			return
		}
		recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		order, err = a.store.FinishWheel(recordCtx, order, receipt, declined)
		if err != nil {
			dateGiftResult(w, nil, gift.ErrPending)
			return
		}
	}
	switch order.State {
	case gift.Pending:
		dateGiftResult(w, nil, gift.ErrPending)
	case gift.Declined:
		writeGiftError(w, http.StatusOK, "4046", payment.ErrInsufficientPoints.Error())
	default:
		dateGiftResult(w, "", nil)
	}
}

func (a wheelAPI) history(w http.ResponseWriter, r *http.Request, actor int64) {
	q := r.URL.Query()
	roomID, roomErr := strconv.ParseInt(q.Get("dateRoomId"), 10, 64)
	page, pageErr := strconv.ParseInt(q.Get("page"), 10, 64)
	size, sizeErr := strconv.ParseInt(q.Get("pageSize"), 10, 64)
	if roomErr != nil || roomID <= 0 || pageErr != nil || sizeErr != nil || page < 1 || page > math.MaxInt32 || size < 1 || size > 100 {
		httpx.Error(w, http.StatusBadRequest, "invalid wheel results page")
		return
	}
	room, err := a.rooms.Info(r.Context(), roomID)
	if err != nil {
		httpx.LiveError(w, err)
		return
	}
	if actor != room.OwnerID {
		if _, err := a.rooms.RTCNID(r.Context(), roomID, actor); err != nil {
			httpx.LiveError(w, err)
			return
		}
	}
	results, pages, err := a.store.WheelHistory(r.Context(), roomID, room.Session.LiveID, page, size)
	if err != nil {
		httpx.LiveError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": results, "pageTotal": pages})
}
