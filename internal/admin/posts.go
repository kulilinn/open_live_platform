package admin

import (
	"context"
	"crypto/sha256"
	"net/http"
	"time"

	"open_live_platform/internal/transport/httpx"
)

type PostReviewQuery struct {
	PageRequest
	AuthorID                                             *int64     `json:"authorId,string"`
	AuthorName                                           *string    `json:"authorName"`
	Status                                               *int8      `json:"passStatus"`
	CreatedFrom                                          *string    `json:"createStime"`
	CreatedTo                                            *string    `json:"createEtime"`
	ReviewedFrom                                         *string    `json:"passStime"`
	ReviewedTo                                           *string    `json:"passEtime"`
	CreatedStart, CreatedEnd, ReviewedStart, ReviewedEnd *time.Time `json:"-"`
}

type PostReviewRow struct {
	ID         int64      `json:"id,string"`
	Author     int64      `json:"authorId,string"`
	AuthorName string     `json:"authorName"`
	Type       *int8      `json:"ty"`
	Visibility int        `json:"level"`
	Likes      int64      `json:"likes"`
	Comments   int64      `json:"commentNum"`
	Price      int64      `json:"price"`
	Status     int        `json:"passStatus"`
	ReviewedAt *time.Time `json:"passTime"`
	CreatedAt  time.Time  `json:"createTime"`
}

type PostReviewDetail struct {
	PostReviewRow
	Content string `json:"content"`
	Media   string `json:"url"`
}

type PostCommentReview struct {
	ID        int64     `json:"id,string"`
	Author    int64     `json:"uid,string"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"create_at"`
}

type PostReviewStore interface {
	BackofficePostPage(context.Context, PostReviewQuery) ([]PostReviewRow, int64, error)
	BackofficePostDetail(context.Context, int64, int64) (PostReviewDetail, error)
	BackofficePostComments(ctx context.Context, id int64, page PageRequest) ([]PostCommentReview, int64, error)
	RemoveBackofficePost(ctx context.Context, operator Principal, path string, id, author int64) error
	RemoveBackofficePostComment(ctx context.Context, operator Principal, path string, id, postID int64) error
}

func (b backoffice) registerPostReviews(mux *http.ServeMux) {
	store, ok := b.store.(PostReviewStore)
	if !ok {
		return
	}
	const prefix = "/api/operation/business/post/"
	add := func(path string, handler backofficeHandler) {
		mux.HandleFunc("POST "+prefix+path, b.authorized(false, true, handler))
	}
	add("page", func(w http.ResponseWriter, r *http.Request, _ Principal, _ [sha256.Size]byte) {
		var input PostReviewQuery
		if !httpx.Decode(w, r, &input) {
			return
		}
		var createOK, reviewOK bool
		input.CreatedStart, input.CreatedEnd, createOK = backofficeDateRange(input.CreatedFrom, input.CreatedTo)
		input.ReviewedStart, input.ReviewedEnd, reviewOK = backofficeDateRange(input.ReviewedFrom, input.ReviewedTo)
		if !createOK || !reviewOK {
			backofficeError(w, http.StatusBadRequest, "400", "Invalid post time range")
			return
		}
		rows, total, err := store.BackofficePostPage(r.Context(), input)
		backofficePage(w, rows, input.PageRequest, total, err)
	})
	add("detail", func(w http.ResponseWriter, r *http.Request, _ Principal, _ [sha256.Size]byte) {
		id, author, ok := backofficePostID(w, r)
		if !ok {
			return
		}
		item, err := store.BackofficePostDetail(r.Context(), id, author)
		backofficeReply(w, item, err)
	})
	add("delete", func(w http.ResponseWriter, r *http.Request, principal Principal, _ [sha256.Size]byte) {
		id, author, ok := backofficePostID(w, r)
		if !ok {
			return
		}
		backofficeReply(w, nil, store.RemoveBackofficePost(r.Context(), principal, r.URL.Path, id, author))
	})
	add("comments", func(w http.ResponseWriter, r *http.Request, _ Principal, _ [sha256.Size]byte) {
		var input struct {
			PageRequest
			PostID *int64 `json:"postId,string"`
		}
		if !httpx.Decode(w, r, &input) {
			return
		}
		if input.PostID == nil || *input.PostID < 0 {
			backofficeError(w, http.StatusBadRequest, "400", "Invalid post ID")
			return
		}
		items, total, err := store.BackofficePostComments(r.Context(), *input.PostID, input.PageRequest)
		current, size, _ := input.Window()
		backofficeReply(w, map[string]any{"ack": map[string]any{"comments": nonNil(items)}, "current": current, "pageSize": size, "total": total}, err)
	})
	add("comment_delete", func(w http.ResponseWriter, r *http.Request, operator Principal, _ [sha256.Size]byte) {
		var input struct {
			ID     int64 `json:"id,string"`
			PostID int64 `json:"postId,string"`
		}
		if !httpx.Decode(w, r, &input) {
			return
		}
		if input.ID <= 0 || input.PostID <= 0 {
			backofficeError(w, http.StatusBadRequest, "400", "Invalid comment or post ID")
			return
		}
		backofficeReply(w, nil, store.RemoveBackofficePostComment(r.Context(), operator, r.URL.Path, input.ID, input.PostID))
	})
}

func backofficePostID(w http.ResponseWriter, r *http.Request) (id, author int64, ok bool) {
	var body struct {
		ID     int64 `json:"id,string"`
		Author int64 `json:"uid,string"`
	}
	if !httpx.Decode(w, r, &body) {
		return 0, 0, false
	}
	if body.ID <= 0 || body.Author < 0 {
		backofficeError(w, http.StatusBadRequest, "400", "Invalid post or author ID")
		return 0, 0, false
	}
	return body.ID, body.Author, true
}

func backofficeDateRange(start, end *string) (*time.Time, *time.Time, bool) {
	if start == nil || end == nil {
		return nil, nil, true
	}
	from, to := parsePurchaseTime(*start), parsePurchaseTime(*end)
	return &from, &to, !from.IsZero() && !to.IsZero() && !to.Before(from)
}
