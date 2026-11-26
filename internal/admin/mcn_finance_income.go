package admin

import (
	"context"
	"crypto/sha256"
	"net/http"
	"time"

	"open_live_platform/internal/transport/httpx"
)

type MCNFinanceIncomeQuery struct {
	AgencyID     *int64  `json:"mcnId,string"`
	AgencyName   *string `json:"mcnName"`
	StreamerName *string `json:"streamerName"`
	Month        string  `json:"month"`
	From         string  `json:"startDate"`
	To           string  `json:"endDate"`
}

type MCNFinanceAgencyIncome struct {
	AgencyID  int64  `json:"mcnId,string"`
	Name      string `json:"mcnName"`
	SuperName string `json:"mcnSuperName"`
	LD        int64  `json:"ld"`
	Ratio     string `json:"ratio"`
}

type MCNFinanceStreamerIncome struct {
	AgencyID     int64   `json:"mcnId,string"`
	AgencyName   *string `json:"mcnName"`
	StreamerName *string `json:"streamerName"`
	LD           int64   `json:"ld"`
	Ratio        string  `json:"ratio"`
	LiveStart    *int64  `json:"liveStartTime"`
	LiveEnd      *int64  `json:"liveEndTime"`
	Date         string  `json:"createDate"`
}

type MCNRefresh struct {
	AgencyID   *int64  `json:"mcnId,string"`
	Start      string  `json:"startTime"`
	End        string  `json:"endTime"`
	StartMonth string  `json:"startMonth"`
	EndMonth   string  `json:"endMonth"`
	Streamers  []int64 `json:"streamerIds"`
}

type MCNFinanceIncomeStore interface {
	MCNFinanceAgencyIncome(context.Context, MCNFinanceIncomeQuery) ([]MCNFinanceAgencyIncome, error)
	MCNFinanceStreamerIncome(context.Context, MCNFinanceIncomeQuery) ([]MCNFinanceStreamerIncome, int64, error)
	RefreshMCNIncome(ctx context.Context, actor Principal, path, kind string, q MCNDateQuery) error
}

func (b backoffice) registerMCNFinanceIncome(mux *http.ServeMux) {
	store, ok := b.store.(MCNFinanceIncomeStore)
	if !ok {
		return
	}
	b.mountMCNFinance(mux, "/income/agency_page", "/income/agency/finance_admin_agency_page", func(w http.ResponseWriter, r *http.Request, _ Principal, _ [sha256.Size]byte) {
		var q MCNFinanceIncomeQuery
		if httpx.Decode(w, r, &q) {
			if _, err := time.Parse("2006-01", q.Month); err != nil || (q.AgencyID != nil && *q.AgencyID <= 0) {
				backofficeError(w, http.StatusBadRequest, "400", "Invalid agency or month")
				return
			}
			rows, err := store.MCNFinanceAgencyIncome(r.Context(), q)
			backofficeReply(w, nonNil(rows), err)
		}
	})
	b.mountMCNFinance(mux, "/income/streamer_page", "/income/agency/finance_admin_streamer_page", func(w http.ResponseWriter, r *http.Request, _ Principal, _ [sha256.Size]byte) {
		var q MCNFinanceIncomeQuery
		if httpx.Decode(w, r, &q) {
			from, fromErr := time.Parse(time.DateOnly, q.From)
			to, toErr := time.Parse(time.DateOnly, q.To)
			if fromErr != nil || toErr != nil || to.Before(from) || (q.AgencyID != nil && *q.AgencyID <= 0) {
				backofficeError(w, http.StatusBadRequest, "400", "Invalid agency or date range")
				return
			}
			rows, total, err := store.MCNFinanceStreamerIncome(r.Context(), q)
			backofficeReply(w, struct {
				Total int64                      `json:"ldTotal"`
				Items []MCNFinanceStreamerIncome `json:"datas"`
			}{total, nonNil(rows)}, err)
		}
	})
	refresh := func(kind string) backofficeHandler {
		return func(w http.ResponseWriter, r *http.Request, actor Principal, _ [sha256.Size]byte) {
			var body MCNRefresh
			if !httpx.Decode(w, r, &body) {
				return
			}
			if actor.Area == MCNArea {
				if !actor.Admin {
					backofficeError(w, http.StatusForbidden, "403", "Agency administrator is required")
					return
				}
				body.AgencyID = &actor.AgencyID
				body.Streamers = nil
			}
			query, valid := parseMCNRefresh(body, kind)
			if !valid {
				backofficeError(w, http.StatusBadRequest, "400", "Invalid agency or refresh period")
				return
			}
			mcnDirectoryResult(w, store.RefreshMCNIncome(r.Context(), actor, r.URL.Path, kind, query))
		}
	}
	b.mountMCNFinance(mux, "/income/refresh_settle_data", "/statistic/streamer/finance_refresh_settle_ld", refresh("daily"))
	if b.area == MCNArea {
		mux.HandleFunc("POST /api/statistic/streamer/refresh_settle_ld", b.authorized(false, true, refresh("daily")))
		mux.HandleFunc("POST /api/statistic/streamer/refresh_settle_ld_streamer_by_month", b.authorized(false, true, refresh("streamer")))
		mux.HandleFunc("POST /api/statistic/streamer/refresh_settle_ld_platform", b.authorized(false, true, refresh("platform")))
	}
}

func parseMCNRefresh(body MCNRefresh, kind string) (MCNDateQuery, bool) {
	var q MCNDateQuery
	if body.AgencyID == nil || *body.AgencyID <= 0 {
		return q, false
	}
	q.AgencyID = *body.AgencyID
	if kind == "daily" {
		start, end := parsePurchaseTime(body.Start), parsePurchaseTime(body.End)
		if start.IsZero() || end.IsZero() {
			return q, false
		}
		if len(body.End) == len(time.DateOnly) {
			end = end.AddDate(0, 0, 1)
		}
		for _, id := range body.Streamers {
			if id <= 0 {
				return q, false
			}
		}
		q.Start, q.End, q.Streamers = &start, &end, body.Streamers
		return q, end.After(start)
	}
	start, end := parseMCNMonth(body.StartMonth), parseMCNMonth(body.EndMonth)
	q.Start, q.End = &start, &end
	return q, !start.IsZero() && !end.IsZero() && !end.Before(start)
}

func parseMCNMonth(value string) time.Time {
	for _, layout := range []string{"2006-01", time.DateOnly} {
		if t, err := time.Parse(layout, value); err == nil {
			return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
		}
	}
	return time.Time{}
}
