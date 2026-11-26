package admin

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"iter"
	"net/http"
	"strconv"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/transport/httpx"
)

type AnchorRecordStore interface {
	AnchorReviewDetail(context.Context, int64, bool) (json.RawMessage, error)
	AnchorIdentityPage(context.Context, PageRequest, *string) ([]json.RawMessage, int64, error)
	CancelAnchorReview(context.Context, Principal, string, int64) error
	RegisterAccounts(context.Context, iter.Seq[auth.ProvisionUser], int64) error
}

func (b backoffice) registerAnchorRecords(mux *http.ServeMux) {
	store, ok := b.store.(AnchorRecordStore)
	if !ok {
		return
	}
	for _, action := range []string{"detail", "kyc_detail", "cancel"} {
		mux.HandleFunc("POST "+anchorReviewPath+action, b.authorized(false, true, func(w http.ResponseWriter, r *http.Request, p Principal, _ [sha256.Size]byte) {
			var request struct {
				ID int64 `json:"id,string"`
			}
			if !httpx.Decode(w, r, &request) {
				return
			}
			if action == "cancel" {
				backofficeReply(w, nil, store.CancelAnchorReview(r.Context(), p, r.URL.Path, request.ID))
				return
			}
			data, err := store.AnchorReviewDetail(r.Context(), request.ID, action == "kyc_detail")
			backofficeReply(w, data, err)
		}))
	}
	mux.HandleFunc("POST "+anchorReviewPath+"kyc_page", b.authorized(false, true, func(w http.ResponseWriter, r *http.Request, _ Principal, _ [sha256.Size]byte) {
		var q struct {
			PageRequest
			Name *string `json:"name"`
		}
		if !httpx.Decode(w, r, &q) {
			return
		}
		list, total, err := store.AnchorIdentityPage(r.Context(), q.PageRequest, q.Name)
		page, size, _ := q.Window()
		backofficeReply(w, map[string]any{"list": list, "total": total, "current": page, "pageSize": size}, err)
	}))
	mux.HandleFunc("POST /api/test/batch_signup", b.authorized(false, true, func(w http.ResponseWriter, r *http.Request, _ Principal, _ [sha256.Size]byte) {
		var q struct {
			Prefix, Mail, Password string
			BeginIndex, Count      int32
			IsAnchor               bool
		}
		if !httpx.Decode(w, r, &q) {
			return
		}
		users := func(yield func(auth.ProvisionUser) bool) {
			for i := range q.Count {
				name := q.Prefix + "_" + strconv.FormatInt(int64(q.BeginIndex)+int64(i), 10)
				if !yield(auth.ProvisionUser{Email: name + "@" + q.Mail, Password: q.Password, DisplayName: name, IsAnchor: q.IsAnchor}) {
					return
				}
			}
		}
		backofficeReply(w, "", store.RegisterAccounts(r.Context(), users, DefaultPayPrice()))
	}))
}
