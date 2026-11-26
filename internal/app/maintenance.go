package app

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"net/http"
	"open_live_platform/internal/admin"
	"open_live_platform/internal/auth"
	"open_live_platform/internal/live"
	"open_live_platform/internal/rtc"
	"open_live_platform/internal/store/postgres"
	"open_live_platform/internal/transport/httpx"
	"os"
	"slices"
	"strconv"
	"strings"
)

func newConfigDiagnostics(file string) (http.HandlerFunc, error) {
	var items []struct {
		ID         int32
		Name, Desc string
		Count      int32
		Key        string
	}
	if file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &items); err != nil {
			return nil, err
		}
	}
	info := map[string]any{"len": len(items)}
	if len(items) > 0 {
		order := []string{}
		pairs := []string{}
		for _, v := range items {
			row := fmt.Sprintf("item { id: %d, name: %q, desc: %q, count: %d, key: %q }", v.ID, v.Name, v.Desc, v.Count, v.Key)
			order = append(order, row)
			pairs = append(pairs, fmt.Sprintf("%d: %s", v.ID, row))
		}
		info["list"] = "[" + strings.Join(order, ", ") + "]"
		info["map"] = "{" + strings.Join(pairs, ", ") + "}"
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		httpx.JSON(w, 200, map[string]any{"status": "200", "data": info})
	}, nil
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
