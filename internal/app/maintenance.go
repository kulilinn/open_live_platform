package app

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/admin"
	"open_live_platform/internal/auth"
	"open_live_platform/internal/live"
	"open_live_platform/internal/rtc"
	"open_live_platform/internal/store/postgres"
	"open_live_platform/internal/transport/httpx"
)

const demoItemTable = "demo_tbitem"

type demoItem struct {
	ID    int32  `json:"id"`
	Name  string `json:"name"`
	Desc  string `json:"desc"`
	Count int32  `json:"count"`
	Key   string `json:"key"`
}

type demoConfigView struct {
	Len  int    `json:"len"`
	List string `json:"list,omitempty"`
	Map  string `json:"map,omitempty"`
}

type configDiagnostics struct {
	view atomic.Pointer[demoConfigView]
}

func newConfigDiagnostics(file string) (*configDiagnostics, error) {
	var items []demoItem
	if file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &items); err != nil {
			return nil, err
		}
	}
	diagnostics := new(configDiagnostics)
	diagnostics.view.Store(renderDemoItems(items))
	return diagnostics, nil
}

func (d *configDiagnostics) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	httpx.JSON(w, http.StatusOK, struct {
		Status string          `json:"status"`
		Data   *demoConfigView `json:"data"`
	}{"200", d.view.Load()})
}

func readLubanDemoItems(value []byte) (*demoConfigView, error) {
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(value, &rows); err != nil {
		return nil, fmt.Errorf("%s: %w", demoItemTable, err)
	}
	if rows == nil {
		return nil, fmt.Errorf("%s must be an array", demoItemTable)
	}
	items := make([]demoItem, 0, len(rows))
	for i, row := range rows {
		var item demoItem
		for _, field := range []struct {
			name string
			into any
		}{{"id", &item.ID}, {"name", &item.Name}, {"desc", &item.Desc}, {"count", &item.Count}, {"key", &item.Key}} {
			raw := row[field.name]
			if len(raw) == 0 || string(raw) == "null" {
				return nil, fmt.Errorf("%s row %d requires %s", demoItemTable, i+1, field.name)
			}
			if err := json.Unmarshal(raw, field.into); err != nil {
				return nil, fmt.Errorf("%s row %d %s: %w", demoItemTable, i+1, field.name, err)
			}
		}
		items = append(items, item)
	}
	return renderDemoItems(items), nil
}

func renderDemoItems(items []demoItem) *demoConfigView {
	view := &demoConfigView{Len: len(items)}
	if len(items) == 0 {
		return view
	}
	list := make([]string, 0, len(items))
	byID := make(map[int32]string, len(items))
	ids := make([]int32, 0, len(items))
	for _, v := range items {
		row := fmt.Sprintf("item { id: %d, name: %q, desc: %q, count: %d, key: %q }", v.ID, v.Name, v.Desc, v.Count, v.Key)
		list = append(list, row)
		if _, ok := byID[v.ID]; !ok {
			ids = append(ids, v.ID)
		}
		byID[v.ID] = row
	}
	slices.Sort(ids)
	pairs := make([]string, 0, len(ids))
	for _, id := range ids {
		pairs = append(pairs, fmt.Sprintf("%d: %s", id, byID[id]))
	}
	view.List, view.Map = "["+strings.Join(list, ", ")+"]", "{"+strings.Join(pairs, ", ")+"}"
	return view
}

func registerMaintenance(mux *http.ServeMux, cfg Config, db *postgres.Store, rooms live.RoomStore, signer rtc.Signer, diagnostics http.HandlerFunc) {
	mux.HandleFunc("GET /testconfig", diagnostics)
	operator := func(fn http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok {
				token = r.Header.Get("X-Inner-Service")
			}
			if cfg.MaintenanceToken == "" || subtle.ConstantTimeCompare([]byte(token), []byte(cfg.MaintenanceToken)) != 1 {
				httpx.Error(w, 403, "operator authorization required")
				return
			}
			fn(w, r)
		}
	}
	mux.HandleFunc("POST /batch_signup", operator(func(w http.ResponseWriter, r *http.Request) {
		if db == nil {
			httpx.Error(w, 503, "account database unavailable")
			return
		}
		var request struct {
			Users []auth.ProvisionUser `json:"users"`
		}
		if !httpx.Decode(w, r, &request) {
			return
		}
		if err := db.RegisterAccounts(r.Context(), slices.Values(request.Users), admin.DefaultPayPrice()); errors.Is(err, auth.ErrMissingFields) {
			httpx.Error(w, 400, err.Error())
			return
		} else if err != nil {
			httpx.Error(w, 500, "batch registration failed")
			return
		}
		w.WriteHeader(200)
	}))
	mux.HandleFunc("POST /api/fill_search_name", operator(func(w http.ResponseWriter, r *http.Request) {
		if db == nil {
			httpx.Error(w, 503, "account database unavailable")
			return
		}
		if err := db.FillSearchNames(r.Context()); err != nil {
			httpx.Error(w, 500, "could not allocate search names")
			return
		}
		httpx.JSON(w, 200, map[string]string{"status": "200"})
	}))
	mux.HandleFunc("GET /anchor/api/delete_anchor/{email}", operator(func(w http.ResponseWriter, r *http.Request) {
		if db == nil {
			httpx.Error(w, 503, "account database unavailable")
			return
		}
		switch err := db.DeleteAnchorAccount(r.Context(), r.PathValue("email")); {
		case errors.Is(err, pgx.ErrNoRows):
			httpx.Error(w, 404, "account not found")
		case errors.Is(err, postgres.ErrAccountLedger):
			httpx.Error(w, 409, err.Error())
		case err != nil:
			httpx.Error(w, 500, "account deletion failed")
		default:
			w.Header().Set("Cache-Control", "no-store")
			httpx.JSON(w, 200, map[string]string{"status": "200"})
		}
	}))
	mux.HandleFunc("GET /api/live/adtmz3gw6u5fnh4qrxsm/{channel}/{role}/{nid}/{uid}", operator(func(w http.ResponseWriter, r *http.Request) {
		roomID, ok := httpx.PathID(w, r, "channel")
		if !ok {
			return
		}
		uid, ok := httpx.PathID(w, r, "uid")
		if !ok {
			return
		}
		requested, err := strconv.ParseUint(r.PathValue("nid"), 10, 32)
		role := r.PathValue("role")
		if err != nil || (role != "1" && role != "2") {
			httpx.Error(w, 400, "invalid RTC identity")
			return
		}
		nid, err := rooms.RTCNID(r.Context(), roomID, uid)
		if err != nil || uint32(requested) != nid || (role == "1" && uid != roomID) {
			httpx.Error(w, 403, "invalid room role or RTC identity")
			return
		}
		if !signer.Configured() {
			httpx.Error(w, 503, "RTC unavailable")
			return
		}
		v, err := signer.ForRoom(roomID, uid, nid, role == "1")
		if err != nil {
			httpx.Error(w, 500, "could not sign RTC token")
			return
		}
		httpx.JSON(w, 200, map[string]any{"status": "200", "data": v})
	}))
}
