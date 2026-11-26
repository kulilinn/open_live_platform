package app_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"open_live_platform/internal/app"
	"open_live_platform/internal/translate"
)

func TestTranslateRouteAuthenticatesAndReturnsOriginalOnMissingProvider(t *testing.T) {
	platform, err := app.New(t.Context(), app.Config{TextTranslation: translate.Config{Qwen: true}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(platform.Close)
	user, err := platform.Accounts.SignUp(t.Context(), "translate@example.org", "password", "Reader")
	if err != nil {
		t.Fatal(err)
	}
	request := func(token, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		incoming := httptest.NewRequest(http.MethodPost, "/api/translate", strings.NewReader(body))
		incoming.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		platform.Server.Handler.ServeHTTP(response, incoming)
		if response.Code != want {
			t.Fatalf("translate HTTP status = %d, want %d", response.Code, want)
		}
		return response
	}
	request("", `{"text":"hello","to":["ja"]}`, http.StatusUnauthorized)
	request(user.Token, `{"text":"hello"}`, http.StatusBadRequest)
	response := request(user.Token, `{"text":"hello","to":["ja","en"],"llm":false}`, http.StatusOK)
	var body struct {
		Status string
		Data   translate.Result
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "200" || body.Data.DetectedLanguage != nil || len(body.Data.Translations) != 2 ||
		body.Data.Translations[0] != (translate.Item{Text: "hello", To: "ja"}) || body.Data.Translations[1] != (translate.Item{Text: "hello", To: "en"}) {
		t.Fatalf("unconfigured provider response = %+v", body)
	}
}
