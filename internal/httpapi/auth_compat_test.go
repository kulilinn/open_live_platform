package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"open_live_platform/internal/auth"
)

func TestRustAuthCompatibility(t *testing.T) {
	s := server{accounts: auth.NewStore()}
	body := []byte("{\"email\":\"rust@example.com\",\"password\":\"secret\",\"account\":\"Rust User\",\"code\":\"ignored\"}")
	req := httptest.NewRequest(http.MethodPost, "/api/auth/signup", bytes.NewReader(body))
	req.Header.Set("Accept", "application/vnd.livemedia+json")
	rec := httptest.NewRecorder()
	s.signUpCompat(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("signup status = %d", rec.Code)
	}
	var signup map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &signup); err != nil {
		t.Fatal(err)
	}
	user := signup["data"].(map[string]any)["user"].(map[string]any)
	if signup["status"] != "200" || user["id"] != "1" || user["language"] != "ja" {
		t.Fatalf("signup response = %+v", signup)
	}

	signinRequest := httptest.NewRequest(http.MethodPost, "/api/auth/signin",
		bytes.NewReader([]byte("{\"email\":\"rust@example.com\",\"password\":\"secret\"}")))
	signinRequest.Header.Set("Accept", "application/vnd.livemedia+json")
	signinRecorder := httptest.NewRecorder()
	s.signInCompat(signinRecorder, signinRequest)
	if signinRecorder.Code != http.StatusOK {
		t.Fatalf("signin status = %d", signinRecorder.Code)
	}
	var signin map[string]any
	if err := json.Unmarshal(signinRecorder.Body.Bytes(), &signin); err != nil {
		t.Fatal(err)
	}
	if signin["status"] != "200" || signin["token"] == "" {
		t.Fatalf("signin response = %+v", signin)
	}
	refresh := signin["token"].(string)
	sessionRequest := httptest.NewRequest(http.MethodGet, "/api/auth/session?compat=rust", nil)
	sessionRequest.Header.Set("Accept", "application/vnd.livemedia+json")
	sessionRequest.Header.Set("Authorization", "Bearer "+refresh)
	sessionRecorder := httptest.NewRecorder()
	s.sessionCompat(sessionRecorder, sessionRequest)
	if sessionRecorder.Code != http.StatusOK {
		t.Fatalf("session status = %d", sessionRecorder.Code)
	}
	var access map[string]any
	if err := json.Unmarshal(sessionRecorder.Body.Bytes(), &access); err != nil {
		t.Fatal(err)
	}
	if access["status"] != "200" || access["token"] == "" || access["token"] == refresh {
		t.Fatalf("session response = %+v", access)
	}
	foundUser, ok, err := s.accounts.Bearer(t.Context(), "Bearer "+access["token"].(string))
	if err != nil || !ok || foundUser.ID != 1 {
		t.Fatalf("access token lookup = %+v, %v, %v", foundUser, ok, err)
	}
}
