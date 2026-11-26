package admin

import (
	"context"
	"crypto/sha256"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"open_live_platform/internal/anchor"
	"open_live_platform/internal/transport/httpx"
)

const anchorReviewPath = "/api/operation/business/anchorreview/"

type AnchorReviewQuery struct {
	PageRequest
	ID    *int64  `json:"id,string"`
	Name  *string `json:"name"`
	Email *string `json:"email"`
	State *int    `json:"state"`
	KYC   *int    `json:"kycStatus"`
	Start *string `json:"createStime"`
	End   *string `json:"createEtime"`
}

type AnchorReviewRow struct {
	ID            int64     `json:"userId,string"`
	Name          string    `json:"name"`
	Mail          string    `json:"mail"`
	Thumbnail     string    `json:"thumbnail"`
	Country       string    `json:"country"`
	Fans          int64     `json:"fansNum"`
	Earnings      int64     `json:"earnLr,string"`
	State         int       `json:"state"`
	KYC           int       `json:"kycStatus"`
	AccountStatus int       `json:"accountStatus"`
	TeacherLabel  string    `json:"teacherLabel"`
	Reason        string    `json:"rejectReason,omitempty"`
	CreatedAt     time.Time `json:"createTime"`
}

type AnchorReviewStore interface {
	BackofficeAnchorReviews(context.Context, AnchorReviewQuery) ([]AnchorReviewRow, int64, error)
	ReviewAnchor(context.Context, Principal, string, int64, int, string, int64) (AnchorReviewRow, error)
	EditTeacherLabel(context.Context, Principal, string, int64, string) error
}

func DefaultPayPrice() int64 {
	price := int64(2990)
	if raw := os.Getenv("DEFAULT_PAY_PRICE"); raw != "" {
		if configured, err := strconv.ParseInt(raw, 10, 32); err == nil && configured > 0 {
			price = configured
		}
	}
	return price
}

func (b backoffice) registerAnchorReviews(mux *http.ServeMux) {
	store, ok := b.store.(AnchorReviewStore)
	if !ok {
		return
	}
	price := DefaultPayPrice()
	mux.HandleFunc("POST "+anchorReviewPath+"edit_teacher_label", b.authorized(false, true,
		func(w http.ResponseWriter, r *http.Request, actor Principal, _ [sha256.Size]byte) {
			var input struct {
				ID    int64   `json:"id,string"`
				Label *string `json:"teacherLabel"`
			}
			if !httpx.Decode(w, r, &input) {
				return
			}
			if input.ID <= 0 || input.Label == nil || utf8.RuneCountInString(*input.Label) > 150 {
				backofficeError(w, http.StatusBadRequest, "400", "Invalid teacher label")
				return
			}
			backofficeReply(w, nil, store.EditTeacherLabel(r.Context(), actor, r.URL.Path, input.ID, *input.Label))
		}))
	mux.HandleFunc("POST "+anchorReviewPath+"page", b.authorized(false, true,
		func(w http.ResponseWriter, r *http.Request, _ Principal, _ [sha256.Size]byte) {
			var query AnchorReviewQuery
			if !httpx.Decode(w, r, &query) {
				return
			}
			for _, date := range []*string{query.Start, query.End} {
				if date != nil && *date != "" {
					if _, err := time.Parse("2006-01-02", *date); err != nil {
						backofficeError(w, http.StatusBadRequest, "400", "Invalid application date")
						return
					}
				}
			}
			items, total, err := store.BackofficeAnchorReviews(r.Context(), query)
			page, size, _ := query.Window()
			backofficeReply(w, map[string]any{"list": items, "total": total, "current": page, "pageSize": size}, err)
		}))
	for name, state := range map[string]int{"pass": anchor.Approved, "reject": anchor.Rejected} {
		mux.HandleFunc("POST "+anchorReviewPath+name, b.authorized(false, true,
			func(w http.ResponseWriter, r *http.Request, p Principal, _ [sha256.Size]byte) {
				var input struct {
					ID     int64  `json:"id,string"`
					Reason string `json:"reason"`
				}
				if !httpx.Decode(w, r, &input) {
					return
				}
				input.Reason = strings.TrimSpace(input.Reason)
				if input.ID <= 0 || (state == anchor.Rejected && input.Reason == "") {
					backofficeError(w, http.StatusBadRequest, "400", "Invalid review request")
					return
				}
				if state == anchor.Approved {
					input.Reason = ""
				}
				result, err := store.ReviewAnchor(r.Context(), p, r.URL.Path, input.ID, state, input.Reason, price)
				backofficeReply(w, result, err)
			}))
	}
}
