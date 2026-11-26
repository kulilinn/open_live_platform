package httpapi

import (
	"errors"
	"net/http"
	"time"

	"open_live_platform/internal/anchor"
	"open_live_platform/internal/transport/httpx"
)

type channelAPI struct {
	auth  *anchor.ChannelAuth
	store anchor.ChannelStore
}

func RegisterAnchorChannelReports(mux *http.ServeMux, authorizer *anchor.ChannelAuth, store anchor.ChannelStore) {
	api := channelAPI{authorizer, store}
	mux.HandleFunc("GET /anchor/api/stat/day", api.stats(true))
	mux.HandleFunc("GET /anchor/api/stat/total", api.stats(false))
	mux.HandleFunc("POST /anchor/api/game/income", api.game)
	mux.HandleFunc("POST /anchor/api/hardware/income", api.hardware)
	mux.HandleFunc("POST /anchor/api/hardware/settlement", api.hardware)
}

func (a channelAPI) channel(w http.ResponseWriter, r *http.Request) (string, bool) {
	channel, ok := a.auth.Channel(r.Header.Get("Authorization"))
	if !ok {
		w.Header().Set("WWW-Authenticate", "Bearer")
		anchorStatus(w, http.StatusUnauthorized, "401", "invalid channel bearer token")
	}
	return channel, ok
}

func (a channelAPI) stats(day bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		channel, ok := a.channel(w, r)
		if !ok {
			return
		}
		filter := anchor.ChannelFilter{Channel: channel}
		if day {
			date := r.URL.Query().Get("search_date")
			if date == "" {
				date = time.Now().UTC().Format(time.DateOnly)
			}
			from, err := time.Parse(time.DateOnly, date)
			if err != nil {
				httpx.Error(w, http.StatusBadRequest, "invalid search_date")
				return
			}
			to := from.AddDate(0, 0, 1)
			filter.From, filter.To = &from, &to
		}
		result, ok := a.report(w, r, filter)
		if !ok {
			return
		}
		registrations := result.Users
		if day {
			registrations = result.NewUsers
		}
		collectibleData(w, map[string]any{"channel": channel, "register_num": registrations, "buyer_num": result.Buyers})
	}
}

func (a channelAPI) game(w http.ResponseWriter, r *http.Request) {
	channel, ok := a.channel(w, r)
	if !ok {
		return
	}
	date, ok := channelDate(w, r)
	if !ok {
		return
	}
	filter := anchor.ChannelFilter{Channel: channel}
	result := anchor.ChannelTotals{}
	switch from, to, valid := channelDateWindow(date); {
	case date == "":
		result, ok = a.report(w, r, filter)
	case valid:
		filter.From, filter.To = &from, &to
		result, ok = a.report(w, r, filter)
	}
	if !ok {
		return
	}
	income, share := a.auth.Amounts(result.Points, result.RevenueRatio)
	collectibleData(w, map[string]any{"users": result.Users, "buy_users": result.Buyers, "number": result.Transactions,
		"income": income, "into": share})
}

func channelDateWindow(date string) (time.Time, time.Time, bool) {
	if from, err := time.Parse(time.DateOnly, date); err == nil {
		return from, from.AddDate(0, 0, 1), true
	}
	if from, err := time.Parse("2006-01", date); err == nil {
		return from, from.AddDate(0, 1, 0), true
	}
	return time.Time{}, time.Time{}, false
}

func channelDate(w http.ResponseWriter, r *http.Request) (string, bool) {
	var input struct {
		Date *string `json:"date"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2048)
	if !httpx.Decode(w, r, &input) {
		return "", false
	}
	if input.Date == nil {
		httpx.Error(w, http.StatusBadRequest, "date is required")
		return "", false
	}
	return *input.Date, true
}

func (a channelAPI) report(w http.ResponseWriter, r *http.Request, filter anchor.ChannelFilter) (anchor.ChannelTotals, bool) {
	if a.store == nil {
		httpx.Error(w, http.StatusServiceUnavailable, anchor.ErrChannelUnavailable.Error())
		return anchor.ChannelTotals{}, false
	}
	result, err := a.store.ChannelReport(r.Context(), filter)
	if errors.Is(err, anchor.ErrChannelUnavailable) {
		httpx.Error(w, http.StatusServiceUnavailable, err.Error())
		return result, false
	}
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "could not read channel report")
		return result, false
	}
	return result, true
}

func (a channelAPI) hardware(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.channel(w, r); !ok {
		return
	}
	if _, ok := channelDate(w, r); !ok {
		return
	}
	// The source hardware operations return these values without a ledger.
	collectibleData(w, map[string]int{"number": 0, "income": 0, "into": 0})
}
