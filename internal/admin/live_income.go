package admin

import (
	"context"
	"crypto/sha256"
	"net/http"
	"strconv"
	"time"

	"open_live_platform/internal/transport/httpx"
)

type LiveIncomeQuery struct {
	PageRequest
	StartText  string    `json:"startTime"`
	EndText    string    `json:"endTime"`
	SearchID   *string   `json:"streamerSearchId"`
	Streamer   *int64    `json:"streamerId,string"`
	Name       *string   `json:"streamerName"`
	MCNText    *string   `json:"mcnId"`
	MCNName    *string   `json:"mcnName"`
	MCN        *int64    `json:"-"`
	Start, End time.Time `json:"-"`
}

type LiveIncomeRow struct {
	SearchID string     `json:"streamerSid"`
	Streamer int64      `json:"streamerUid,string"`
	Name     string     `json:"streamerName"`
	MCN      *int64     `json:"mcnId,string"`
	MCNName  *string    `json:"mcnName"`
	Start    *time.Time `json:"liveStartTime"`
	End      *time.Time `json:"liveEndTime"`
	Minutes  int64      `json:"liveDuration"`
	Diamond  string     `json:"diamond"`
	Created  string     `json:"created"`
}

type LiveIncomeStore interface {
	BackofficeLiveIncome(context.Context, LiveIncomeQuery) ([]LiveIncomeRow, int64, error)
}

func (b backoffice) registerLiveIncome(mux *http.ServeMux) {
	store, ok := b.store.(LiveIncomeStore)
	if !ok {
		return
	}
	mux.HandleFunc("POST /api/finance/purchase/streamer_live_income", b.authorizedTenant(false, true, FinanceTenant,
		func(w http.ResponseWriter, r *http.Request, _ Principal, _ [sha256.Size]byte) {
			var query LiveIncomeQuery
			if !httpx.Decode(w, r, &query) {
				return
			}
			query.Start, query.End = parsePurchaseTime(query.StartText), parsePurchaseTime(query.EndText)
			if query.Start.IsZero() || query.End.IsZero() || query.End.Before(query.Start) {
				backofficeError(w, http.StatusBadRequest, "400", "invalid broadcast time range")
				return
			}
			if query.MCNText != nil {
				id, err := strconv.ParseInt(*query.MCNText, 10, 64)
				if err != nil || id <= 0 {
					backofficeError(w, http.StatusBadRequest, "400", "invalid MCN ID")
					return
				}
				query.MCN = &id
			}
			rows, total, err := store.BackofficeLiveIncome(r.Context(), query)
			backofficePage(w, rows, query.PageRequest, total, err)
		}))
}
