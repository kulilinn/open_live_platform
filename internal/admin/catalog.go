package admin

import (
	"crypto/sha256"
	"net/http"
	"strconv"
	"time"

	"open_live_platform/internal/gift"
	"open_live_platform/internal/transport/httpx"
)

type PurchaseQuery struct {
	PageRequest
	ReceiverID *int64     `json:"receiverId,string"`
	SenderID   *int64     `json:"senderId,string"`
	TradeNo    *int64     `json:"tradeNo,string"`
	TradeType  *int       `json:"tradeTy"`
	GiftName   *string    `json:"giftName"`
	StartText  string     `json:"startTime"`
	EndText    string     `json:"endTime"`
	Start, End *time.Time `json:"-"`
}

type PurchaseRow struct {
	Points    int64     `json:"point"`
	Receiver  int64     `json:"receiverId,string"`
	Sender    int64     `json:"senderId,string"`
	TradeNo   string    `json:"tradeNo"`
	TradeType int       `json:"tradeTy"`
	Income    int64     `json:"ld"`
	BadDebt   int64     `json:"badDebtPoint"`
	BadIncome int64     `json:"badDebtLd"`
	Subject   string    `json:"desc"`
	Created   time.Time `json:"createTime"`
}

func (b backoffice) registerCatalogAndFinance(mux *http.ServeMux, catalog *gift.Catalog) {
	b.registerGiftTemplates(mux, catalog)
	const base = "POST /api/operation/gifts/gift/"
	mux.HandleFunc(base+"list", b.authorized(false, true, func(w http.ResponseWriter, r *http.Request, _ Principal, _ [sha256.Size]byte) {
		items, err := catalog.Items(r.Context())
		if err != nil {
			backofficeReply(w, nil, err)
			return
		}
		list := make([]map[string]string, 0, len(items))
		for _, item := range items {
			list = append(list, map[string]string{"id": strconv.FormatInt(item.ID, 10), "name": item.Name})
		}
		backofficeReply(w, list, nil)
	}))
	mux.HandleFunc(base+"page", b.authorized(false, true, func(w http.ResponseWriter, r *http.Request, _ Principal, _ [sha256.Size]byte) {
		var query struct {
			PageRequest
			ID   *int64  `json:"id,string"`
			Name *string `json:"name"`
			Type *int8   `json:"ty"`
		}
		if !httpx.Decode(w, r, &query) {
			return
		}
		items, err := catalog.Items(r.Context())
		if err != nil {
			backofficeReply(w, nil, err)
			return
		}
		matched := make([]map[string]any, 0)
		for _, item := range items {
			if (query.ID != nil && item.ID != *query.ID) || (query.Type != nil && item.GiftType != *query.Type) ||
				(query.Name != nil && item.Name != *query.Name) {
				continue
			}
			var sort int32
			if item.GiftSort != nil {
				sort = *item.GiftSort
			}
			matched = append(matched, map[string]any{"id": strconv.FormatInt(item.ID, 10), "name": item.Name, "icon": item.Icon,
				"ty": item.GiftType, "sort": sort, "price": strconv.FormatInt(item.Price, 10)})
		}
		_, size, offset := query.Window()
		total := int64(len(matched))
		offset = min(offset, total)
		backofficePage(w, matched[offset:min(total, offset+size)], query.PageRequest, total, nil)
	}))
	mux.HandleFunc("POST /api/finance/purchase/page", b.authorizedTenant(false, true, FinanceTenant, func(w http.ResponseWriter, r *http.Request, _ Principal, _ [sha256.Size]byte) {
		var query PurchaseQuery
		if !httpx.Decode(w, r, &query) {
			return
		}
		if query.StartText != "" && query.EndText != "" {
			start, end := parsePurchaseTime(query.StartText), parsePurchaseTime(query.EndText)
			if start.IsZero() || end.IsZero() || end.Before(start) {
				backofficeError(w, http.StatusBadRequest, "400", "invalid purchase time range")
				return
			}
			query.Start, query.End = &start, &end
		}
		list, total, err := b.store.BackofficeGiftPurchases(r.Context(), query)
		backofficePage(w, list, query.PageRequest, total, err)
	}))
}

func parsePurchaseTime(value string) time.Time {
	for _, format := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
		if parsed, err := time.Parse(format, value); err == nil {
			return parsed
		}
	}
	return time.Time{}
}
