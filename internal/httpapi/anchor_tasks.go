package httpapi

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strconv"
	"time"

	"open_live_platform/internal/anchor"
	"open_live_platform/internal/auth"
	"open_live_platform/internal/live"
	"open_live_platform/internal/transport/httpx"
)

type anchorTaskAPI struct {
	server
	records anchor.Store
	tasks   anchor.TaskStore
	token   string
}

func RegisterAnchorTasks(mux *http.ServeMux, accounts auth.AccountStore, rooms live.RoomStore, profiles anchor.Store, tasks anchor.TaskStore, token string) {
	s := anchorTaskAPI{server: server{accounts: accounts, rooms: rooms}, records: profiles, tasks: tasks, token: token}
	mux.HandleFunc("GET /anchor/api/query_task_progress", s.authenticated(s.query))
	mux.HandleFunc("GET /anchor/api/test_task", s.authenticated(s.reconcile))
}

func (s anchorTaskAPI) query(w http.ResponseWriter, r *http.Request, actor int64) {
	if s.tasks == nil {
		httpx.Error(w, http.StatusServiceUnavailable, "anchor task storage is unavailable")
		return
	}
	result, err := s.tasks.Tasks(r.Context(), actor, time.Now())
	if err != nil {
		anchorError(w, err)
		return
	}
	writeSubscriptionData(w, result)
}

func (s anchorTaskAPI) reconcile(w http.ResponseWriter, r *http.Request, actor int64) {
	if s.token == "" || subtle.ConstantTimeCompare([]byte(s.token), []byte(r.Header.Get("X-Anchor-Task-Token"))) != 1 {
		httpx.Error(w, http.StatusForbidden, "anchor task maintenance is forbidden")
		return
	}
	if s.tasks == nil {
		httpx.Error(w, http.StatusServiceUnavailable, "anchor task storage is unavailable")
		return
	}
	ctx := r.Context()
	record, err := s.records.Anchor(ctx, actor)
	if errors.Is(err, anchor.ErrNotFound) {
		_, err = (subscriptionAPI{server: s.server}).anchor(ctx, actor)
	} else if err == nil && record.State != anchor.Approved {
		err = anchor.ErrPending
	}
	if err != nil {
		httpx.Error(w, http.StatusForbidden, "anchor task maintenance is forbidden")
		return
	}
	query := r.URL.Query()
	target, err := strconv.Atoi(query.Get("task_target"))
	_, qtyErr := strconv.ParseInt(query.Get("complete_target_quantity"), 10, 64)
	if err != nil || target < 0 || target >= int(anchor.TaskTargetCount) || qtyErr != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid task parameters")
		return
	}
	anchorMutation(w, s.tasks.ReconcileTasks(ctx, actor, time.Now()))
}
