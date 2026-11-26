package admin

import (
	"context"
	"crypto/sha256"
	"net/http"
	"time"

	"open_live_platform/internal/transport/httpx"
)

type UserHistoryQuery struct {
	PageRequest
	UID        int64      `json:"uid,string"`
	Status     *int8      `json:"status"`
	Type       *int8      `json:"ty"`
	StartText  *string    `json:"startTime"`
	EndText    *string    `json:"endTime"`
	Start, End *time.Time `json:"-"`
}

type ChargeHistoryRow struct {
	ID      int64     `json:"orderId,string"`
	Price   string    `json:"price"`
	Points  int64     `json:"lp"`
	Method  string    `json:"payMethod"`
	Status  int8      `json:"status"`
	Created time.Time `json:"createTime"`
}

type ConsumeHistoryRow struct {
	Type    int8      `json:"ty"`
	Points  int64     `json:"lp"`
	Name    *string   `json:"name"`
	Created time.Time `json:"createTime"`
}

type UserHistoryStore interface {
	BackofficeChargeHistory(context.Context, UserHistoryQuery) ([]ChargeHistoryRow, int64, error)
	BackofficeConsumeHistory(context.Context, UserHistoryQuery) ([]ConsumeHistoryRow, int64, error)
}

func (b backoffice) registerUserHistory(mux *http.ServeMux) {
	store, ok := b.store.(UserHistoryStore)
	if !ok {
		return
	}
	for name, charge := range map[string]bool{"charge_records": true, "consume_records": false} {
		mux.HandleFunc("POST /api/operation/usermanage/user/"+name, b.authorized(false, true,
			func(w http.ResponseWriter, r *http.Request, _ Principal, _ [sha256.Size]byte) {
				var query UserHistoryQuery
				if !httpx.Decode(w, r, &query) {
					return
				}
				if query.UID <= 0 {
					backofficeError(w, http.StatusBadRequest, "400", "Invalid user ID")
					return
				}
				if query.StartText != nil && query.EndText != nil {
					from, to := parsePurchaseTime(*query.StartText), parsePurchaseTime(*query.EndText)
					if from.IsZero() || to.IsZero() || to.Before(from) {
						backofficeError(w, http.StatusBadRequest, "400", "Invalid history time range")
						return
					}
					query.Start, query.End = &from, &to
				}
				if charge {
					items, total, err := store.BackofficeChargeHistory(r.Context(), query)
					backofficePage(w, items, query.PageRequest, total, err)
				} else {
					items, total, err := store.BackofficeConsumeHistory(r.Context(), query)
					backofficePage(w, items, query.PageRequest, total, err)
				}
			}))
	}
}
