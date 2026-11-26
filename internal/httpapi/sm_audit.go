package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"open_live_platform/internal/posts"
	"open_live_platform/internal/transport/httpx"
)

func (s postAPI) registerSMAudit(mux *http.ServeMux) {
	mux.HandleFunc("POST /sm/media/callback", s.reviewSMMedia(false))
	mux.HandleFunc("POST /sm/media_manual/callback", s.reviewSMMedia(true))
	mux.HandleFunc("POST /sm/static/callback", s.reviewSMStatic)
	mux.HandleFunc("POST /sm/public_chat/audit", s.authenticated(s.publicChatAudit))
}

func (s postAPI) reviewSMStatic(w http.ResponseWriter, r *http.Request) {
	if !httpx.RequireToken(w, r, s.options.ReviewToken) {
		return
	}
	var input struct {
		RequestID string `json:"requestId"`
		Result    *struct {
			Operation int `json:"operation"`
		} `json:"result"`
		Data *struct {
			Subject *struct {
				UserID      subscriptionID `json:"userId"`
				CommentID   subscriptionID `json:"id"`
				ContentType int            `json:"contentType"`
				Content     string         `json:"content"`
			} `json:"passThrough"`
		} `json:"data"`
	}
	if !decodePost(w, r, &input) {
		return
	}
	if input.RequestID == "" || input.Result == nil || input.Result.Operation < 1 || input.Data == nil || input.Data.Subject == nil {
		postError(w, posts.ErrInvalid)
		return
	}
	if input.Result.Operation == 1 {
		smAuditAcknowledged(w)
		return
	}
	const postCommentType = 12
	subject := input.Data.Subject
	if subject.ContentType != postCommentType {
		httpx.Error(w, http.StatusUnprocessableEntity, "unsupported static audit action")
		return
	}
	if subject.UserID < 1 || subject.CommentID < 1 || subject.Content == "" {
		postError(w, posts.ErrInvalid)
		return
	}
	err := s.entries.RejectPostComment(r.Context(), int64(subject.UserID), int64(subject.CommentID), subject.Content, input.RequestID)
	if err != nil && !errors.Is(err, posts.ErrNotFound) {
		postError(w, err)
		return
	}
	smAuditAcknowledged(w)
}

func (s postAPI) publicChatAudit(w http.ResponseWriter, r *http.Request, actor int64) {
	var input struct {
		Content string `json:"content"`
	}
	if !decodePost(w, r, &input) {
		return
	}
	if strings.TrimSpace(input.Content) == "" {
		postError(w, posts.ErrInvalid)
		return
	}
	if s.options.Reviewer == nil && !s.options.AuditRequired {
		postStatus(w, nil)
		return
	}
	if s.options.Reviewer == nil {
		httpx.Error(w, http.StatusServiceUnavailable, "public chat review is unavailable")
		return
	}
	status, err := s.options.Reviewer.Review(r.Context(), posts.ReviewRequest{Kind: "public_chat", UserID: actor, Text: input.Content})
	switch {
	case err != nil || status == posts.Pending:
		httpx.Error(w, http.StatusServiceUnavailable, "public chat review is unavailable")
	case status == posts.Rejected:
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "4068"})
	default:
		postStatus(w, nil)
	}
}

type smPassThrough struct {
	UserID subscriptionID `json:"userId"`
	DataID subscriptionID `json:"dataId"`
}

func (s postAPI) reviewSMMedia(manual bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !httpx.RequireToken(w, r, s.options.ReviewToken) {
			return
		}
		var input struct {
			RequestID string        `json:"requestId"`
			Risk      string        `json:"riskLevel"`
			Post      smPassThrough `json:"passThrough"`
		}
		if !decodePost(w, r, &input) {
			return
		}
		if input.RequestID == "" || input.Post.UserID < 1 || input.Post.DataID < 1 {
			postError(w, posts.ErrInvalid)
			return
		}
		decision := posts.ReviewDecision{Notice: posts.NoticeMachineRejection}
		if manual {
			decision.Notice = posts.NoticeManualRejection
		}
		switch input.Risk {
		case "PASS":
			decision.Status = posts.Approved
		case "REJECT":
			decision.Status = posts.Rejected
		case "REVIEW":
			decision.Status = posts.Pending
		default:
			postError(w, posts.ErrInvalid)
			return
		}
		ctx := r.Context()
		id, owner := int64(input.Post.DataID), int64(input.Post.UserID)
		matched, err := s.entries.MatchPostReview(ctx, id, owner, input.RequestID, manual)
		if err != nil {
			postError(w, err)
			return
		}
		if !matched {
			httpx.Error(w, http.StatusNotFound, "post review target not found")
			return
		}
		if decision.Status != posts.Pending {
			if err := s.entries.ReviewPost(ctx, id, decision); err != nil {
				postError(w, err)
				return
			}
		}
		smAuditAcknowledged(w)
	}
}

func smAuditAcknowledged(w http.ResponseWriter) {
	httpx.JSON(w, http.StatusOK, map[string]any{"code": 1100, "message": "success"})
}
