package httpapi

import (
	"net/http"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/transport/httpx"
)

func (s server) changePassword(w http.ResponseWriter, r *http.Request, userID int64) {
	var input struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
		OldCamel    string `json:"oldPassword"`
		NewCamel    string `json:"newPassword"`
	}
	if !httpx.Decode(w, r, &input) {
		return
	}
	if input.OldPassword == "" {
		input.OldPassword = input.OldCamel
	}
	if input.NewPassword == "" {
		input.NewPassword = input.NewCamel
	}
	err := s.accounts.ChangePassword(r.Context(), userID, input.OldPassword, input.NewPassword)
	switch err {
	case nil:
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "200"})
	case auth.ErrMissingFields:
		httpx.Error(w, http.StatusBadRequest, err.Error())
	case auth.ErrInvalidCredentials:
		httpx.Error(w, http.StatusUnauthorized, err.Error())
	default:
		httpx.Error(w, http.StatusInternalServerError, "could not change password")
	}
}
