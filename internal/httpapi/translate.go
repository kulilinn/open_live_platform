package httpapi

import (
	"net/http"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/translate"
	"open_live_platform/internal/transport/httpx"
)

func RegisterTranslate(mux *http.ServeMux, accounts auth.AccountStore, translator *translate.Service) {
	s := server{accounts: accounts}
	mux.HandleFunc("POST /api/translate", s.authenticated(func(w http.ResponseWriter, r *http.Request, _ int64) {
		var input struct {
			Text *string
			To   []string
			LLM  *bool
		}
		if !httpx.Decode(w, r, &input) {
			return
		}
		if input.Text == nil || input.To == nil {
			httpx.Error(w, http.StatusBadRequest, "text and to are required")
			return
		}
		result := translator.Translate(r.Context(), translate.Request{Text: *input.Text, To: input.To, LLM: input.LLM})
		httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": result})
	}))
}
