package admin

import (
	"context"
	"crypto/sha256"
	"net/http"
	"strconv"
	"time"

	"open_live_platform/internal/transport/httpx"
)

type MCNIncomeQuery struct {
	PageRequest
	AgencyID  int64
	ManagerID *int64
	Streamers []int64
	From, To  *string
}

type MCNStreamerIncome struct {
	AgencyID      int64   `json:"mcnId,string"`
	ManagerID     *int64  `json:"managerId,string"`
	ManagerName   *string `json:"managerName"`
	StreamerID    int64   `json:"streamerId,string"`
	StreamerName  *string `json:"streamerName"`
	StreamerEmail *string `json:"streamerEmail"`
	LD            int64   `json:"ld"`
	StarCoinTotal *int64  `json:"starCoinTotal,string"`
	LiveFlowTotal *int64  `json:"liveFlowTotal"`
	Ratio         *string `json:"ratio"`
	ManagerRatio  *string `json:"managerRatio"`
	AgencyRatio   *string `json:"agencyRatio"`
	LiveStart     *int64  `json:"liveStartTime"`
	LiveEnd       *int64  `json:"liveEndTime"`
	Day           string  `json:"createTime"`
}

type MCNIncomeSplit struct {
	ManagerID       *int64  `json:"managerId,string"`
	StreamerID      int64   `json:"streamerId,string"`
	StreamerName    *string `json:"streamerName"`
	LD              int64   `json:"ld"`
	Ratio           string  `json:"ratio"`
	ManagerRatio    string  `json:"managerRatio"`
	AgencyRatio     string  `json:"agencyRatio"`
	Day             string  `json:"createDate"`
	RoleName        *string `json:"roleName"`
	RoleManagerName *string `json:"roleManagerName"`
}

type MCNMonthlyIncome struct {
	LD    int64  `json:"ld"`
	Ratio string `json:"ratio"`
}

type MCNIncomeStore interface {
	MCNAgency(context.Context, int64) (MCNAgency, error)
	MCNStreamerIncome(context.Context, MCNIncomeQuery) ([]MCNStreamerIncome, int64, error)
	MCNIncomeSplits(context.Context, MCNIncomeQuery) ([]MCNIncomeSplit, int64, error)
	MCNMonthlyIncome(ctx context.Context, agency int64, month string) ([]MCNMonthlyIncome, error)
	MCNPlatformRatio(context.Context, MCNDateQuery) (string, error)
}

func (b backoffice) registerMCNIncome(mux *http.ServeMux) {
	store, ok := b.store.(MCNIncomeStore)
	if !ok {
		return
	}
	mux.HandleFunc("POST /api/income/agency/info", b.authorized(false, true, func(w http.ResponseWriter, r *http.Request, actor Principal, _ [sha256.Size]byte) {
		data, err := store.MCNAgency(r.Context(), actor.AgencyID)
		backofficeReply(w, map[string]int64{"total": data.RosterCount, "num": data.StreamerCount}, err)
	}))
	mux.HandleFunc("POST /api/income/agency/lr_settle", b.authorized(false, true, func(w http.ResponseWriter, r *http.Request, actor Principal, _ [sha256.Size]byte) {
		var body struct {
			Month string `json:"month"`
		}
		if !httpx.Decode(w, r, &body) {
			return
		}
		if _, err := time.Parse("2006-01", body.Month); err != nil {
			backofficeError(w, http.StatusBadRequest, "400", "Invalid month")
			return
		}
		items, err := store.MCNMonthlyIncome(r.Context(), actor.AgencyID, body.Month)
		backofficeReply(w, nonNil(items), err)
	}))
	mux.HandleFunc("POST /api/income/agency/fetch_platform_ratio", b.authorized(false, true, func(w http.ResponseWriter, r *http.Request, actor Principal, _ [sha256.Size]byte) {
		var body struct {
			Start *string `json:"startDate"`
			End   *string `json:"endDate"`
		}
		if !httpx.Decode(w, r, &body) {
			return
		}
		query, valid := mcnDateQuery(actor.AgencyID, nil, nil, body.Start, body.End)
		if !valid {
			backofficeError(w, http.StatusBadRequest, "400", "Invalid income date range")
			return
		}
		ratio, err := store.MCNPlatformRatio(r.Context(), query)
		backofficeReply(w, ratio, err)
	}))
	for _, path := range []string{"agency_page", "ratio_page"} {
		mux.HandleFunc("POST /api/income/streamer/"+path, b.authorized(false, true, func(w http.ResponseWriter, r *http.Request, actor Principal, _ [sha256.Size]byte) {
			var body struct {
				PageRequest
				ManagerID *int64   `json:"managerId,string"`
				Streamers []string `json:"streamerIds"`
				From      *string  `json:"startTime"`
				To        *string  `json:"endTime"`
			}
			if !httpx.Decode(w, r, &body) {
				return
			}
			ids, valid := mcnStreamerIDs(body.Streamers)
			manager := mcnManagerScope(actor, body.ManagerID)
			if !valid || (manager != nil && *manager <= 0) || !mcnIncomeDates(body.From, body.To) {
				backofficeError(w, http.StatusBadRequest, "400", "Invalid income period or streamer")
				return
			}
			query := MCNIncomeQuery{PageRequest: body.PageRequest, AgencyID: actor.AgencyID, ManagerID: manager,
				Streamers: ids, From: body.From, To: body.To}
			if path == "ratio_page" {
				items, total, err := store.MCNIncomeSplits(r.Context(), query)
				backofficePage(w, items, query.PageRequest, total, err)
				return
			}
			items, total, err := store.MCNStreamerIncome(r.Context(), query)
			backofficePage(w, items, query.PageRequest, total, err)
		}))
	}
}

func mcnIncomeDates(from, to *string) bool {
	if from == nil || to == nil {
		return from == nil && to == nil
	}
	for _, layout := range []string{time.DateOnly, "2006-01"} {
		start, firstErr := time.Parse(layout, *from)
		end, lastErr := time.Parse(layout, *to)
		if firstErr == nil && lastErr == nil {
			return !end.Before(start)
		}
	}
	return false
}

func mcnStreamerIDs(values []string) ([]int64, bool) {
	ids := make([]int64, 0, len(values))
	for _, value := range values {
		id, err := strconv.ParseInt(value, 10, 64)
		if err != nil || id <= 0 {
			return nil, false
		}
		ids = append(ids, id)
	}
	return ids, true
}
