package httpapi

import (
	"net/http"
	"strconv"

	"open_live_platform/internal/transport/httpx"
)

func (s server) userProfile(w http.ResponseWriter, r *http.Request, currentID int64) {
	targetID := currentID
	if value := r.PathValue("user_id"); value != "" && value != "me" {
		var err error
		targetID, err = strconv.ParseInt(value, 10, 64)
		if err != nil || targetID < 1 {
			httpx.JSON(w, http.StatusOK, map[string]string{"status": "4005", "message": "data not found "})
			return
		}
	}
	user, ok, err := s.accounts.Find(r.Context(), targetID)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "could not read user")
		return
	}
	if !ok {
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "4005", "message": "data not found "})
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"status": "200",
		"data": map[string]any{
			"id": strconv.FormatInt(user.ID, 10), "name": user.Account,
			"gender": "0", "photo": "", "verified": false, "online": 0,
			"popularity": 0, "xkey": "", "rkey": "", "userType": "normal",
			"pt": 0, "promotion": "", "nsfw": nil, "isAnchor": false,
			"toyInfo": "{}", "anchorStatus": 0, "banner": "",
			"displayName": user.Account, "searchName": user.Account,
			"groupRoom": "", "level": 0, "exp": 0, "maxExp": 0,
			"trans": false, "transLang": "", "limitTy": 0, "limitAllow": true,
			"limitTotal": 0, "limitTime": 0, "limitReason": []string{},
			"fansTotal": 0, "teacherLabel": "", "email": user.Email,
			"language": "ja", "search_name": user.Account, "token_id": nil,
			"equipped_adorn": map[string]any{},
		},
	})
}
