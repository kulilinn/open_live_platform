package admin

import (
	"context"
	"crypto/sha256"
	"net/http"
	"strconv"
	"time"

	"open_live_platform/internal/transport/httpx"
)

type MCNAgency struct {
	Name          *string `json:"name"`
	StreamerTotal int64   `json:"streamerTotal"`
	StreamerCount int64   `json:"streamerNum"`
	ManagerCount  int64   `json:"managerNum"`
	AdminCount    int64   `json:"adminNum"`
	FanCount      int64   `json:"fansNum"`
	RosterCount   int64   `json:"-"`
}

type MCNDateQuery struct {
	AgencyID   int64
	ManagerID  *int64
	Streamers  []int64
	Start, End *time.Time
}

type MCNPerformance struct {
	LD, ViewerCount, NewFans *int64
	StreamerCount            int64
}

type MCNChartRow struct {
	Day         string `json:"createTime"`
	LD          int64  `json:"ld"`
	ViewerCount int64  `json:"viewerNum"`
}

type MCNStreamerPerformance struct {
	Income       int64      `json:"income"`
	ViewerCount  *int64     `json:"viewerCount"`
	NewFans      *int64     `json:"newFansNum"`
	Start        *time.Time `json:"startTime"`
	End          *time.Time `json:"endTime"`
	StreamerID   int64      `json:"streamerId,string"`
	StreamerName string     `json:"streamerName"`
	ManagerName  *string    `json:"managerName"`
}

type MCNMessage struct {
	ID        int64     `json:"id"`
	Type      int       `json:"ty"`
	UserID    int64     `json:"user_id"`
	Params    string    `json:"params"`
	CreatedAt time.Time `json:"create_time"`
}

type MCNReportStore interface {
	MCNAgency(context.Context, int64) (MCNAgency, error)
	MCNPerformance(context.Context, MCNDateQuery) (MCNPerformance, error)
	MCNLiveChart(context.Context, MCNDateQuery) ([]MCNChartRow, error)
	MCNStreamerPerformance(context.Context, MCNDateQuery) ([]MCNStreamerPerformance, error)
	MCNApplicationStatus(context.Context, int64, int64, *int64) (int, error)
	MCNMessages(context.Context, int64, *int64) ([]MCNMessage, error)
}

func (b backoffice) registerMCNReports(mux *http.ServeMux) {
	store, ok := b.store.(MCNReportStore)
	if !ok {
		return
	}
	mux.HandleFunc("POST /api/home/agency_data", b.authorized(false, false, func(w http.ResponseWriter, r *http.Request, p Principal, _ [sha256.Size]byte) {
		agency, err := store.MCNAgency(r.Context(), p.AgencyID)
		for _, member := range b.principals {
			if member.Area == MCNArea && member.AgencyID == p.AgencyID && !member.Disabled {
				if member.Admin {
					agency.AdminCount++
				} else if member.Manager {
					agency.ManagerCount++
				}
			}
		}
		backofficeReply(w, agency, err)
	}))
	mux.HandleFunc("POST /api/home/message", b.authorized(false, false, func(w http.ResponseWriter, r *http.Request, p Principal, _ [sha256.Size]byte) {
		items, err := store.MCNMessages(r.Context(), p.AgencyID, mcnManagerScope(p, nil))
		backofficeReply(w, nonNil(items), err)
	}))
	mux.HandleFunc("POST /api/home/agency_data_yesterday", b.authorized(false, false, func(w http.ResponseWriter, r *http.Request, p Principal, _ [sha256.Size]byte) {
		var body struct {
			ID *string `json:"id"`
		}
		if !httpx.Decode(w, r, &body) {
			return
		}
		query, valid := mcnDateQuery(p.AgencyID, nil, nil, body.ID, body.ID)
		if !valid {
			backofficeError(w, http.StatusBadRequest, "400", "Invalid performance date")
			return
		}
		data, err := store.MCNPerformance(r.Context(), query)
		if err == nil && data.LD == nil {
			err = ErrBackofficeNotFound
		}
		backofficeReply(w, struct {
			LD          *string `json:"ld"`
			ViewerCount *string `json:"viewerNum"`
			NewFans     *string `json:"newFansNum"`
		}{mcnDecimal(data.LD), mcnDecimal(data.ViewerCount), mcnDecimal(data.NewFans)}, err)
	}))
	mux.HandleFunc("POST /api/statistic/live/yesterday", b.authorized(false, true, func(w http.ResponseWriter, r *http.Request, p Principal, _ [sha256.Size]byte) {
		var body struct {
			Date      *string `json:"date"`
			ManagerID *int64  `json:"managerId"`
		}
		if !httpx.Decode(w, r, &body) {
			return
		}
		query, valid := mcnDateQuery(p.AgencyID, mcnManagerScope(p, body.ManagerID), nil, body.Date, body.Date)
		if !valid || body.Date == nil {
			backofficeError(w, http.StatusBadRequest, "400", "Invalid performance date or manager")
			return
		}
		data, err := store.MCNPerformance(r.Context(), query)
		backofficeReply(w, struct {
			Income      *int64 `json:"ld"`
			ViewerCount *int64 `json:"viewerNum"`
			NewFans     *int64 `json:"newFansNum"`
			Count       int64  `json:"count"`
		}{data.LD, data.ViewerCount, data.NewFans, data.StreamerCount}, err)
	}))
	for _, path := range []string{"line_chart", "streamer_list"} {
		mux.HandleFunc("POST /api/statistic/live/"+path, b.authorized(false, true, func(w http.ResponseWriter, r *http.Request, p Principal, _ [sha256.Size]byte) {
			var body struct {
				ManagerID *int64   `json:"managerId,string"`
				Streamers []string `json:"streamerIds"`
				Start     *string  `json:"startTime"`
				End       *string  `json:"endTime"`
			}
			if !httpx.Decode(w, r, &body) {
				return
			}
			ids, validIDs := mcnStreamerIDs(body.Streamers)
			if !validIDs {
				backofficeError(w, http.StatusBadRequest, "400", "Invalid streamer ID")
				return
			}
			query, valid := mcnDateQuery(p.AgencyID, mcnManagerScope(p, body.ManagerID), ids, body.Start, body.End)
			if path == "streamer_list" && body.Start != nil && body.End != nil {
				start, end := parsePurchaseTime(*body.Start), parsePurchaseTime(*body.End)
				query.Start, query.End = &start, &end
				valid = !start.IsZero() && !end.IsZero() && !end.Before(start) && (query.ManagerID == nil || *query.ManagerID > 0)
			}
			if !valid || (path == "line_chart" && query.Start == nil) {
				backofficeError(w, http.StatusBadRequest, "400", "Invalid performance date range")
				return
			}
			if path == "line_chart" {
				rows, err := store.MCNLiveChart(r.Context(), query)
				backofficeReply(w, nonNil(rows), err)
				return
			}
			rows, err := store.MCNStreamerPerformance(r.Context(), query)
			backofficeReply(w, nonNil(rows), err)
		}))
	}
	mux.HandleFunc("POST /api/mcn/streamer/apply/fetch_status", b.authorized(false, false, func(w http.ResponseWriter, r *http.Request, p Principal, _ [sha256.Size]byte) {
		var body struct {
			AgencyID   int64 `json:"mcnId,string"`
			StreamerID int64 `json:"streamerId,string"`
		}
		if !httpx.Decode(w, r, &body) {
			return
		}
		if body.AgencyID != p.AgencyID || (!p.Admin && !p.Manager) {
			backofficeError(w, http.StatusForbidden, "403", "Operation not allowed")
			return
		}
		if body.StreamerID <= 0 {
			backofficeError(w, http.StatusBadRequest, "400", "Invalid streamer ID")
			return
		}
		status, err := store.MCNApplicationStatus(r.Context(), p.AgencyID, body.StreamerID, mcnManagerScope(p, nil))
		backofficeReply(w, status, err)
	}))
}

func mcnManagerScope(actor Principal, requested *int64) *int64 {
	if actor.Admin {
		return requested
	}
	return &actor.ID
}

func mcnDecimal(value *int64) *string {
	if value == nil {
		return nil
	}
	decimal := strconv.FormatInt(*value, 10)
	return &decimal
}

func mcnDateQuery(agencyID int64, managerID *int64, streamers []int64, from, to *string) (MCNDateQuery, bool) {
	query := MCNDateQuery{AgencyID: agencyID, ManagerID: managerID, Streamers: streamers}
	if managerID != nil && *managerID <= 0 {
		return query, false
	}
	if from == nil || to == nil {
		return query, from == nil && to == nil
	}
	start, startErr := time.Parse(time.DateOnly, *from)
	end, endErr := time.Parse(time.DateOnly, *to)
	query.Start, query.End = &start, &end
	return query, startErr == nil && endErr == nil && !end.Before(start)
}
