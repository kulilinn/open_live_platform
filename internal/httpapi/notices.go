package httpapi

import (
	"net/http"
	"strconv"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/notice"
	"open_live_platform/internal/transport/httpx"
)

func RegisterNotices(mux *http.ServeMux, accounts auth.AccountStore, store notice.Store) {
	s := server{accounts: accounts}
	reply := func(w http.ResponseWriter, data any, err error) {
		if err != nil {
			httpx.Error(w, http.StatusInternalServerError, "could not access notices")
			return
		}
		response := map[string]any{"status": "200"}
		if data != nil {
			response["data"] = data
		}
		httpx.JSON(w, http.StatusOK, response)
	}
	mux.HandleFunc("GET /api/notice/page_notice", s.authenticated(func(w http.ResponseWriter, r *http.Request, id int64) {
		params := r.URL.Query()
		page, e1 := strconv.ParseInt(params.Get("page"), 10, 32)
		size, e2 := strconv.ParseInt(params.Get("pageSize"), 10, 32)
		if e1 != nil || e2 != nil {
			httpx.Error(w, http.StatusBadRequest, "page and pageSize must be integers")
			return
		}
		result, err := store.Notices(r.Context(), id, int32(max(1, page)), int32(max(1, min(100, size))))
		reply(w, result, err)
	}))
	mux.HandleFunc("GET /api/notice/read_notice", s.authenticated(func(w http.ResponseWriter, r *http.Request, id int64) {
		reply(w, nil, store.ReadNotices(r.Context(), id))
	}))
	mux.HandleFunc("GET /api/notice/get_unread_notice_count", s.authenticated(func(w http.ResponseWriter, r *http.Request, id int64) {
		count, err := store.UnreadNotices(r.Context(), id)
		reply(w, count, err)
	}))
}
