package httpapi

import (
	"cmp"
	"crypto/rand"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/transport/httpx"
	"open_live_platform/internal/upload"
)

func RegisterUploads(mux *http.ServeMux, accounts auth.AccountStore, cfg upload.Config, parts upload.Store) {
	s := server{accounts: accounts}
	cfg.ChunkSize = cmp.Or(cfg.ChunkSize, 256000)
	cfg.MaxImage = cmp.Or(cfg.MaxImage, 20<<20)
	cfg.MaxVideo = cmp.Or(cfg.MaxVideo, 300<<20)
	const tooLarge = "uploaded file too large"
	mux.HandleFunc("GET /api/guid", s.authenticated(func(w http.ResponseWriter, r *http.Request, uid int64) {
		n, err := rand.Int(rand.Reader, big.NewInt(1<<53-1))
		if err != nil {
			httpx.Error(w, 500, "could not issue file ID")
			return
		}
		id := n.Int64() + 1
		var token *string
		if suffix := r.URL.Query().Get("fileSuffix"); suffix != "" {
			if !upload.ValidName(suffix) {
				httpx.Error(w, 400, "invalid file suffix")
				return
			}
			if !cfg.Ready() {
				httpx.Error(w, 503, "object storage unavailable")
				return
			}
			v := upload.Token((upload.Part{UserID: uid, Name: strconv.FormatInt(id, 10) + "." + suffix}).Key())
			token = &v
		}
		httpx.JSON(w, 200, map[string]any{"status": "200", "data": id, "chunkSize": cfg.ChunkSize, "maxImageSize": cfg.MaxImage, "maxVideoSize": cfg.MaxVideo, "uploadId": token})
	}))
	mux.HandleFunc("POST /api/upload", s.authenticated(func(w http.ResponseWriter, r *http.Request, uid int64) {
		if !cfg.Ready() {
			httpx.Error(w, 503, "object storage unavailable")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 10<<20)
		if err := r.ParseMultipartForm(256 << 10); err != nil {
			httpx.Error(w, 400, "invalid multipart request")
			return
		}
		defer r.MultipartForm.RemoveAll()
		name := r.PostFormValue("fileid")
		idx, e1 := strconv.ParseInt(r.PostFormValue("chunk_index"), 10, 32)
		count, e2 := strconv.ParseInt(r.PostFormValue("chunk_number"), 10, 32)
		if !upload.ValidName(name) || e1 != nil || e2 != nil || idx < 0 || idx >= count {
			httpx.Error(w, 400, "invalid file or chunk index")
			return
		}
		p := upload.Part{UserID: uid, Name: name, Index: int(idx), Count: int(count), Limit: cfg.Limit(name)}
		if token := r.PostFormValue("upload_id"); token != "" && token != upload.Token(p.Key()) {
			httpx.Error(w, 403, "upload ID does not match this file")
			return
		}
		if count > p.Limit/cfg.ChunkSize {
			httpx.JSON(w, 200, map[string]string{"status": "4053", "message": tooLarge})
			return
		}
		file, _, err := r.FormFile("chunk")
		if err != nil {
			httpx.Error(w, 400, "chunk required")
			return
		}
		defer file.Close()
		p.Data, err = io.ReadAll(file)
		if err != nil {
			httpx.Error(w, 400, "incomplete chunk")
			return
		}
		err = parts.StageUpload(r.Context(), p, cfg.Put)
		switch {
		case errors.Is(err, upload.ErrLimit):
			httpx.JSON(w, 200, map[string]string{"status": "4053", "message": tooLarge})
		case errors.Is(err, upload.ErrConflict):
			httpx.Error(w, 409, err.Error())
		case err != nil:
			httpx.Error(w, 503, "could not save upload")
		default:
			fileURL := strings.TrimRight(cfg.BaseURL, "/") + "/assets/files/" + strconv.FormatInt(uid, 10) + "/" + url.PathEscape(name)
			httpx.JSON(w, 200, map[string]any{"status": "200", "data": map[string]any{"url": fileURL, "chunk_index": idx, "success": true}})
		}
	}))
}
