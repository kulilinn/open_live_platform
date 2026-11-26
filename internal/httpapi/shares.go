package httpapi

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"html/template"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/feed"
	"open_live_platform/internal/transport/httpx"
	"open_live_platform/internal/upload"
)

func RegisterShares(mux *http.ServeMux, accounts auth.AccountStore, shares feed.ShareStore, objects upload.Config) {
	s := server{accounts: accounts}
	result := func(w http.ResponseWriter, data any, err error) {
		if errors.Is(err, feed.ErrShareMissing) {
			httpx.Error(w, 403, "share unavailable")
			return
		}
		if err != nil {
			httpx.Error(w, 500, "could not save share")
			return
		}
		httpx.JSON(w, 200, map[string]any{"status": "200", "data": data})
	}
	write := func(update bool) userHandler {
		return func(w http.ResponseWriter, r *http.Request, uid int64) {
			if !objects.Ready() {
				httpx.Error(w, 503, "object storage unavailable")
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 6<<20)
			if err := r.ParseMultipartForm(256 << 10); err != nil {
				httpx.Error(w, 400, "invalid share form")
				return
			}
			defer r.MultipartForm.RemoveAll()
			ctx := r.Context()
			var id int64
			if update {
				var err error
				id, err = strconv.ParseInt(r.PostFormValue("id"), 10, 64)
				if err != nil {
					httpx.Error(w, 400, "invalid share ID")
					return
				}
				owned, err := shares.OwnsShare(ctx, uid, id)
				if err != nil {
					result(w, nil, err)
					return
				}
				if !owned {
					result(w, nil, feed.ErrShareMissing)
					return
				}
			}
			content, exists := r.MultipartForm.Value["content"]
			f, h, err := r.FormFile("photo")
			if err != nil {
				httpx.Error(w, 400, "photo required")
				return
			}
			defer f.Close()
			if !exists || len(content) == 0 || h.Size > 5<<20 {
				httpx.Error(w, 400, "invalid photo or content")
				return
			}
			body, err := io.ReadAll(f)
			if err != nil {
				httpx.Error(w, 400, "could not read photo")
				return
			}
			kind, _, _ := mime.ParseMediaType(h.Header.Get("Content-Type"))
			ext := "png"
			if _, sub, ok := strings.Cut(kind, "/"); ok && upload.ValidName(sub) {
				ext = sub
			}
			name := fmt.Sprintf("%x.%s", sha256.Sum256(body), ext)
			if err := objects.PutBytes(ctx, "assets/shares/"+name, body); err != nil {
				httpx.Error(w, 503, "could not upload photo")
				return
			}
			if update {
				result(w, "OK", shares.ChangeShare(ctx, uid, id, content[0], name, false))
				return
			}
			v, err := shares.CreateShare(ctx, uid, content[0], name)
			result(w, v, err)
		}
	}
	mux.HandleFunc("POST /api/shares/add", s.authenticated(write(false)))
	mux.HandleFunc("POST /api/shares/update", s.authenticated(write(true)))
	mux.HandleFunc("POST /api/shares/delete", s.authenticated(func(w http.ResponseWriter, r *http.Request, uid int64) {
		var body struct {
			ID int64 `json:"id,string"`
		}
		if httpx.Decode(w, r, &body) {
			result(w, "OK", shares.ChangeShare(r.Context(), uid, body.ID, "", "", true))
		}
	}))
	mux.HandleFunc("POST /api/shares/list", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Offset     int64
			Share, UID string
		}
		if !httpx.Decode(w, r, &body) {
			return
		}
		if body.Offset < 0 {
			httpx.Error(w, 400, "invalid offset")
			return
		}
		var owner int64
		if body.Share == "my" {
			var err error
			owner, err = strconv.ParseInt(body.UID, 10, 64)
			if err != nil || owner < 1 {
				httpx.Error(w, 400, "invalid owner ID")
				return
			}
		}
		list, err := shares.ListShares(r.Context(), body.Offset, body.Share == "hot", owner)
		if err != nil {
			result(w, nil, err)
			return
		}
		for i := range list {
			list[i].Photo = strings.TrimRight(objects.BaseURL, "/") + "/assets/shares/" + url.PathEscape(list[i].Photo)
		}
		httpx.JSON(w, 200, map[string]any{"status": "200", "data": list, "cursor": body.Offset + int64(len(list))})
	})
	mux.HandleFunc("GET /share", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		target, err := url.Parse(q.Get("redirect"))
		if err != nil || target.IsAbs() && (target.Scheme != "http" && target.Scheme != "https") {
			httpx.Error(w, 400, "invalid redirect")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = sharePreview.Execute(w, q.Get)
	})
}

var sharePreview = template.Must(template.New("share").Parse(`<!doctype html><html><head><title>LoveRose</title>
<meta property="og:title" content="{{call . "title"}}"><meta property="og:description" content="{{call . "description"}}">
<meta property="og:image" content="{{call . "image"}}"><meta property="og:url" content="{{call . "url"}}">
<meta property="og:type" content="article"><meta property="og:site_name" content="Loverose"><meta property="fb:app_id" content="{{call . "fb_id"}}">
<meta name="twitter:card" content="summary_large_image"><meta name="twitter:site" content="@{{call . "xsite"}}">
<meta name="twitter:creator" content="@{{call . "xcreator"}}"><meta name="twitter:title" content="{{call . "title"}}">
<meta name="twitter:description" content="{{call . "description"}}"><meta name="twitter:image" content="{{call . "image"}}">
{{with call . "redirect"}}<script>window.addEventListener("load",function(){location.href={{.}}})</script>{{end}}</head><body></body></html>`))
