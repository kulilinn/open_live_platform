package relay

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/transport/httpx"
	"open_live_platform/internal/upload"
)

func (s *rustTickets) deviceLogs(accounts auth.AccountStore, storage upload.Config) http.HandlerFunc {
	const maxLogBytes = 5 << 20
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxLogBytes+(64<<10))
		if err := r.ParseMultipartForm(64 << 10); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				httpx.Error(w, http.StatusRequestEntityTooLarge, "device log exceeds 5 MiB")
				return
			}
			httpx.Error(w, http.StatusBadRequest, "invalid device log multipart request")
			return
		}
		defer r.MultipartForm.RemoveAll()

		fileID := r.PostFormValue("file")
		if !upload.ValidName(fileID) || !upload.ValidName(s.appKey) {
			httpx.Error(w, http.StatusBadRequest, "invalid device log file ID")
			return
		}
		ticket, err := s.verify(r.Context(), r.PostFormValue("token"), r.PostFormValue("sig"), time.Now())
		if err != nil {
			httpx.Error(w, http.StatusServiceUnavailable, "relay tickets unavailable")
			return
		}
		if ticket == nil {
			rustForbidden(w)
			return
		}
		user, ok, err := s.loginUser(r.Context(), accounts, ticket)
		if err != nil {
			httpx.Error(w, http.StatusServiceUnavailable, "relay account unavailable")
			return
		}
		if !ok {
			rustForbidden(w)
			return
		}
		if !storage.Ready() {
			httpx.Error(w, http.StatusServiceUnavailable, "object storage unavailable")
			return
		}
		file, header, err := r.FormFile("data")
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, "device log data is required")
			return
		}
		defer file.Close()
		if header.Size > maxLogBytes {
			httpx.Error(w, http.StatusRequestEntityTooLarge, "device log exceeds 5 MiB")
			return
		}
		data, err := io.ReadAll(io.LimitReader(file, maxLogBytes+1))
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, "incomplete device log data")
			return
		}
		if len(data) > maxLogBytes {
			httpx.Error(w, http.StatusRequestEntityTooLarge, "device log exceeds 5 MiB")
			return
		}
		key := "toylogs/" + s.appKey + "/" + strconv.FormatInt(user.ID, 10) + "/" + fileID
		err = storage.PutBytes(r.Context(), key, data)
		if err != nil {
			httpx.Error(w, http.StatusServiceUnavailable, "could not save device log")
			return
		}
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "200"})
	}
}
