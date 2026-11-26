package httpapi

import (
	"errors"
	"io"
	"net/http"

	"open_live_platform/internal/transport/httpx"
	"open_live_platform/internal/upload"
)

func RegisterWalletPass(mux *http.ServeMux, passes *upload.Passes) {
	mux.HandleFunc("GET /pass/{pass_key}", func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("pass_key")
		file, err := passes.Open(r.Context(), key)
		if errors.Is(err, upload.ErrPassNotFound) {
			httpx.JSON(w, http.StatusOK, map[string]string{"status": "4005", "message": "data not found "})
			return
		}
		if err != nil {
			httpx.Error(w, http.StatusBadGateway, "wallet pass unavailable")
			return
		}
		defer file.Close()
		w.Header().Set("Content-Type", "application/vnd.apple.pkpass")
		w.Header().Set("Content-Disposition", "attachment; filename="+key+".pkpass")
		w.Header().Set("Cache-Control", "no-store")
		io.Copy(w, file)
	})
}
