package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"open_live_platform/internal/date"
	"open_live_platform/internal/gift"
	"open_live_platform/internal/payment"
	"open_live_platform/internal/transport/httpx"
)

type giftInteractionsAPI struct {
	dates   *Dates
	catalog *gift.Catalog
	content gift.Interactions
	billing *payment.Client
}

func RegisterGiftInteractions(mux *http.ServeMux, dates *Dates, catalog *gift.Catalog, content gift.Interactions, billing *payment.Client) {
	a := giftInteractionsAPI{dates, catalog, content, billing}
	s := server{accounts: dates.Accounts}
	mux.HandleFunc("POST /api/gift/block", s.authenticated(a.block))
	mux.HandleFunc("POST /api/gift/recommend/{channel}", s.authenticated(a.recommend))
	mux.HandleFunc("GET /api/gift/promotions/info", s.authenticated(a.promotions))
	mux.HandleFunc("GET /api/gift/box/info", s.authenticated(a.box))
	mux.HandleFunc("GET /api/date/gift/quick/list", s.authenticated(a.quickList))
	mux.HandleFunc("GET /api/gift/game/list", s.authenticated(a.questions))
	mux.HandleFunc("POST /api/gift/game/verif", s.authenticated(a.verifyGame))
	mux.HandleFunc("POST /api/gift/game/accept", s.authenticated(a.voteGame))
	mux.HandleFunc("POST /api/gift/game/quan", s.authenticated(a.moveGame(0)))
	mux.HandleFunc("POST /api/gift/game/guess", s.authenticated(a.moveGame(1)))
}

func (a giftInteractionsAPI) block(w http.ResponseWriter, r *http.Request, actor int64) {
	var input struct {
		Channel int64    `json:"dateRoomId,string"`
		GiftIDs []string `json:"giftIds"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if !httpx.Decode(w, r, &input) {
		return
	}
	if input.Channel <= 0 || input.GiftIDs == nil || len(input.GiftIDs) > 200 {
		httpx.Error(w, http.StatusBadRequest, "invalid gift block")
		return
	}
	ids := make([]int64, 0, len(input.GiftIDs))
	for _, raw := range input.GiftIDs {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			httpx.Error(w, http.StatusBadRequest, "invalid gift ID")
			return
		}
		ids = append(ids, id)
	}
	store, ok := a.dates.Service.Store.(gift.BlockStore)
	if !ok {
		httpx.Error(w, http.StatusServiceUnavailable, "gift block store unavailable")
		return
	}
	dateGiftResult(w, "success", store.BlockDateGifts(r.Context(), input.Channel, actor, ids))
}

func (a giftInteractionsAPI) recommend(w http.ResponseWriter, r *http.Request, actor int64) {
	channel, ok := httpx.PathID(w, r, "channel")
	if !ok {
		return
	}
	var input struct {
		ID int64 `json:"giftId,string"`
	}
	if !httpx.Decode(w, r, &input) {
		return
	}
	ctx := r.Context()
	session, err := a.dates.Service.SessionFor(ctx, channel, actor, true)
	if err == nil && session.AnchorID != actor {
		err = date.ErrForbidden
	}
	if err != nil {
		dateGiftResult(w, nil, err)
		return
	}
	item, ok, err := a.catalog.Lookup(ctx, input.ID)
	if err != nil {
		dateGiftResult(w, nil, err)
		return
	}
	if !ok {
		dateGiftResult(w, nil, gift.ErrOrderNotFound)
		return
	}
	if err := a.checkBlocked(r, channel, item.ID); err != nil {
		dateGiftResult(w, nil, err)
		return
	}
	if a.dates.Push == nil {
		httpx.Error(w, http.StatusServiceUnavailable, "private delivery unavailable")
		return
	}
	payload := map[string]any{"gift": date.GiftView(date.GiftOrder{Item: item}, item.ID, 0, time.Time{}), "quantity": 1}
	if err := a.dates.Push(ctx, session.AudienceID, "recommend_gifts", payload); err != nil {
		httpx.Error(w, http.StatusServiceUnavailable, "private delivery unavailable")
		return
	}
	collectibleData(w, "OK")
}

func (a giftInteractionsAPI) promotionContent(ctx context.Context, actor int64) (gift.Interactions, error) {
	if limits, ok := a.dates.Service.Store.(gift.LimitStore); ok {
		return limits.GiftPromotionContent(ctx, actor, a.content)
	}
	return a.content, nil
}

func (a giftInteractionsAPI) promotions(w http.ResponseWriter, r *http.Request, actor int64) {
	if r.URL.Query().Get("date_room_id") == "" {
		httpx.Error(w, http.StatusBadRequest, "date_room_id is required")
		return
	}
	switch r.URL.Query().Get("all") {
	case "", "true":
		items, err := a.catalog.Items(r.Context())
		if err != nil {
			httpx.Error(w, http.StatusInternalServerError, "could not read gift promotions")
			return
		}
		content, err := a.promotionContent(r.Context(), actor)
		if err != nil {
			httpx.Error(w, http.StatusInternalServerError, "could not read gift promotions")
			return
		}
		collectibleData(w, content.PromotionList(items, time.Now()))
	case "false":
		collectibleData(w, []gift.PromotionView{})
	default:
		httpx.Error(w, http.StatusBadRequest, "all must be true or false")
	}
}

func (a giftInteractionsAPI) box(w http.ResponseWriter, r *http.Request, actor int64) {
	q := r.URL.Query()
	box, boxErr := strconv.ParseInt(q.Get("box_id"), 10, 64)
	user, userErr := strconv.ParseInt(q.Get("user_id"), 10, 64)
	if boxErr != nil || userErr != nil || box <= 0 || user <= 0 {
		httpx.Error(w, http.StatusBadRequest, "invalid gift box query")
		return
	}
	if actor != user {
		dateGiftResult(w, nil, date.ErrForbidden)
		return
	}
	items, err := a.catalog.Items(r.Context())
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "could not read gift box")
		return
	}
	content, err := a.promotionContent(r.Context(), actor)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "could not read gift box")
		return
	}
	collectibleData(w, content.BoxContents(box, actor, items, time.Now()))
}

func (a giftInteractionsAPI) questions(w http.ResponseWriter, _ *http.Request, _ int64) {
	collectibleData(w, a.content.ActiveQuestions())
}

func (a giftInteractionsAPI) verifyGame(w http.ResponseWriter, r *http.Request, actor int64) {
	var input struct {
		Channel int64 `json:"dateRoomId,string"`
		GiftID  int64 `json:"giftId,string"`
		UserID  int64 `json:"userId,string"`
	}
	if !httpx.Decode(w, r, &input) {
		return
	}
	ctx := r.Context()
	session, err := a.dates.Service.SessionFor(ctx, input.Channel, actor, true)
	if err == nil && (input.UserID != actor || session.AudienceID != actor) {
		err = date.ErrForbidden
	}
	if err != nil {
		dateGiftResult(w, nil, err)
		return
	}
	item, ok, err := a.catalog.Lookup(ctx, input.GiftID)
	if err != nil {
		dateGiftResult(w, nil, err)
		return
	}
	if !ok || item.GiftType != 1 {
		dateGiftResult(w, nil, gift.ErrOrderNotFound)
		return
	}
	if err := a.checkBlocked(r, input.Channel, item.ID); err != nil {
		dateGiftResult(w, nil, err)
		return
	}
	cost, err := date.GiftCost(date.GiftInput{Quantity: 1}, item)
	if err == nil && cost > 0 {
		var balance int64
		balance, err = a.billing.Balance(ctx, actor)
		if err == nil && balance < cost {
			err = payment.ErrInsufficientPoints
		}
	}
	if games, ok := a.dates.Service.Store.(date.GiftGameStore); err == nil && ok {
		err = games.PrepareDateGiftGame(ctx, input.Channel, actor)
	}
	dateGiftResult(w, "success", err)
}

func (a giftInteractionsAPI) checkBlocked(r *http.Request, channel, giftID int64) error {
	store, ok := a.dates.Service.Store.(gift.BlockStore)
	if !ok {
		return nil
	}
	blocks, err := store.BlockedDateGifts(r.Context(), channel)
	if err != nil {
		return err
	}
	if _, blocked := blocks[giftID]; blocked {
		return date.ErrForbidden
	}
	return nil
}
