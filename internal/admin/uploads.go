package admin

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"

	"open_live_platform/internal/gift"
	"open_live_platform/internal/upload"
)

type giftSheetWriter interface {
	ApplyGiftSheet(context.Context, Principal, string, string, GiftSheetBatch) error
}

func (b backoffice) registerUploads(mux *http.ServeMux, catalog *gift.Catalog) {
	mux.HandleFunc("POST /api/operation/platform/upload/images", b.authorizedTenant(false, true, "", func(w http.ResponseWriter, r *http.Request, _ Principal, _ [sha256.Size]byte) {
		cfg := b.options.Images
		if !cfg.Ready() {
			backofficeError(w, 503, "503", "Image storage unavailable")
			return
		}
		form, err := r.MultipartReader()
		if err != nil {
			backofficeError(w, 400, "400", "Invalid upload")
			return
		}
		part, err := form.NextPart()
		if err != nil {
			backofficeError(w, 400, "400", "Image required")
			return
		}
		defer part.Close()
		name := part.FileName()
		typ, _, err := mime.ParseMediaType(part.Header.Get("Content-Type"))
		if err != nil || !strings.HasPrefix(typ, "image/") || !upload.ValidName(name) {
			backofficeError(w, 400, "400", "Invalid image")
			return
		}
		data, err := io.ReadAll(part)
		if err != nil || len(data) == 0 {
			backofficeError(w, 400, "400", "Invalid image")
			return
		}
		key := path.Join(b.options.ImagePrefix, time.Now().UTC().Format("20060102"), name)
		cfg.Parent = ""
		if err := cfg.PutTypedBytes(r.Context(), key, typ, data); err != nil {
			backofficeError(w, 502, "502", "Could not upload image")
			return
		}
		backofficeReply(w, key, nil)
	}))
	store, ok := b.store.(giftSheetWriter)
	if !ok {
		return
	}
	for _, endpoint := range []string{"/api/operation/platform/upload/config_file", "/api/operation/gifts/gift/upload_file"} {
		mux.HandleFunc("POST "+endpoint, b.authorized(false, true, func(w http.ResponseWriter, r *http.Request, p Principal, _ [sha256.Size]byte) {
			form, err := r.MultipartReader()
			if err != nil {
				backofficeError(w, 400, "400", "Expected a workbook")
				return
			}
			file, err := form.NextPart()
			if err != nil {
				backofficeError(w, 400, "400", "Missing workbook")
				return
			}
			defer file.Close()
			kind := strings.TrimSuffix(file.FileName(), path.Ext(file.FileName()))
			batch, err := readGiftSheet(r.Context(), kind, file, b.options.LegacyWorkbookReader)
			if errors.Is(err, errXLSBReaderUnavailable) {
				backofficeError(w, http.StatusServiceUnavailable, "503", err.Error())
				return
			}
			if err != nil {
				backofficeError(w, 400, "400", err.Error())
				return
			}
			if _, err = form.NextPart(); err != io.EOF {
				backofficeError(w, 400, "400", "Expected one workbook")
				return
			}
			for _, item := range batch.Gifts {
				if catalog != nil && catalog.Baseline(item.ID) {
					backofficeError(w, 409, "409", "Catalog entry already exists")
					return
				}
			}
			err = store.ApplyGiftSheet(r.Context(), p, endpoint, kind, batch)
			if errors.Is(err, gift.ErrLimit) {
				backofficeError(w, 400, "400", "Invalid promotion limit")
				return
			}
			if errors.Is(err, ErrGiftTemplateExists) {
				backofficeError(w, 409, "409", "Catalog entry already exists")
				return
			}
			backofficeReply(w, nil, err)
		}))
	}
}
