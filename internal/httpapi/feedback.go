package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"open_live_platform/internal/auth"
	"open_live_platform/internal/feedback"
	"open_live_platform/internal/transport/httpx"
	"open_live_platform/internal/upload"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

func RegisterFeedback(mux *http.ServeMux, accounts auth.AccountStore, store feedback.Store, storage upload.Config, hook string) {
	s := server{accounts: accounts}
	mux.HandleFunc("POST /api/feedback/submit", s.authenticated(func(w http.ResponseWriter, r *http.Request, uid int64) {
		r.Body = http.MaxBytesReader(w, r.Body, 6<<20)
		if err := r.ParseMultipartForm(256 << 10); err != nil {
			httpx.Error(w, 400, "invalid feedback form")
			return
		}
		defer r.MultipartForm.RemoveAll()
		kind, err := strconv.ParseUint(r.PostFormValue("feedback_type"), 10, 8)
		if err != nil || len(r.MultipartForm.Value["content"]) == 0 {
			httpx.Error(w, 400, "feedback_type and content required")
			return
		}
		record := feedback.Record{UserID: uid, Content: r.PostFormValue("content"), Type: uint8(kind), Status: 1}
		if utf8.RuneCountInString(record.Content) > 1000 {
			httpx.Error(w, 500, "反馈内容不能超过1000个字")
			return
		}
		var data []byte
		var key string
		if f, header, err := r.FormFile("attachment"); err == nil {
			defer f.Close()
			if !storage.Ready() {
				httpx.Error(w, 503, "object storage unavailable")
				return
			}
			if header.Size > 5<<20 {
				httpx.Error(w, 400, "attachment too large")
				return
			}
			data, err = io.ReadAll(f)
			if err != nil {
				httpx.Error(w, 400, "could not read attachment")
				return
			}
			kind, _, _ := mime.ParseMediaType(header.Header.Get("Content-Type"))
			ext := "png"
			if _, sub, ok := strings.Cut(kind, "/"); ok && upload.ValidName(sub) {
				ext = sub
			}
			key = fmt.Sprintf("assets/feedback/%x.%s", sha256.Sum256(data), ext)
			record.Attachment = strings.TrimRight(storage.BaseURL, "/") + "/" + key
		} else if !errors.Is(err, http.ErrMissingFile) {
			httpx.Error(w, 400, "invalid attachment")
			return
		}
		ctx := r.Context()
		u, ok, err := accounts.Find(ctx, uid)
		if err != nil || !ok {
			httpx.Error(w, 500, "could not find account")
			return
		}
		id, err := store.ReserveFeedback(ctx, record)
		if errors.Is(err, feedback.ErrTooOften) {
			httpx.Error(w, 500, err.Error())
			return
		}
		if err != nil {
			httpx.Error(w, 500, "could not save feedback")
			return
		}
		if key != "" {
			if err = storage.PutBytes(ctx, key, data); err != nil {
				finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				defer cancel()
				_ = store.DiscardFeedback(finish, id)
				httpx.Error(w, 503, "could not upload feedback attachment")
				return
			}
		}
		status := 0
		if hook != "" {
			status = 3
			if sendFeedback(ctx, hook, u.Email, record) {
				status = 2
			}
		}
		finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := store.FinishFeedback(finish, id, status); err != nil {
			httpx.Error(w, 500, "could not record notification status")
			return
		}
		httpx.JSON(w, 200, map[string]string{"status": "200", "data": "success"})
	}))
}
func sendFeedback(ctx context.Context, hook, email string, record feedback.Record) bool {
	labels := []string{"其他", "改进建议", "付款/打赏问题", "技术问题"}
	label := labels[0]
	if int(record.Type) < len(labels) {
		label = labels[record.Type]
	}
	line := []map[string]string{{"tag": "text", "text": "用户：" + email + "\n"}, {"tag": "text", "text": "【" + label + "】\n"}, {"tag": "text", "text": record.Content}}
	if record.Attachment != "" {
		line = append(line, map[string]string{"tag": "a", "text": "\n请查看附件", "href": record.Attachment})
	}
	data, err := json.Marshal(map[string]any{"msg_type": "post", "content": map[string]any{"post": map[string]any{"zh_cn": map[string]any{"title": "收到新反馈", "content": [][]map[string]string{line}}}}})
	if err != nil {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, "POST", hook, bytes.NewReader(data))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	client := http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var reply struct{ Code *int }
	return resp.StatusCode/100 == 2 && json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&reply) == nil && reply.Code != nil && *reply.Code == 0
}
