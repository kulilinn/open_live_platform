package app_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"open_live_platform/internal/app"
)

func TestGiftCatalog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gifts.json")
	catalog := `[{"id":"9007199254740993","name":"Flower","price":1},{"id":"2","name":"Star","price":3,"giftSort":1}]`
	if err := os.WriteFile(path, []byte(catalog), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := app.New(t.Context(), app.Config{GiftCatalogFile: path})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	session, err := a.Accounts.SignUp(t.Context(), "gift@example.test", "password", "Gift viewer")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		query string
		token string
		code  int
		ids   []string
	}{
		{"", "", http.StatusUnauthorized, nil},
		{"", session.Token, http.StatusOK, []string{"2", "9007199254740993"}},
		{"?all=true", session.Token, http.StatusOK, []string{"2", "9007199254740993"}},
		{"?all=false", session.Token, http.StatusOK, []string{}},
		{"?all=invalid", session.Token, http.StatusBadRequest, nil},
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/gift/list"+tc.query, nil)
		if tc.token != "" {
			req.Header.Set("Authorization", "Bearer "+tc.token)
		}
		rec := httptest.NewRecorder()
		a.Server.Handler.ServeHTTP(rec, req)
		if rec.Code != tc.code {
			t.Fatalf("%s returned %d, want %d", tc.query, rec.Code, tc.code)
		}
		if tc.code == http.StatusOK {
			var response struct {
				Status string
				Data   []struct{ ID string }
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			ids := make([]string, 0, len(response.Data))
			for _, item := range response.Data {
				ids = append(ids, item.ID)
			}
			if response.Status != "200" || response.Data == nil || !slices.Equal(ids, tc.ids) {
				t.Fatalf("%s response = %s", tc.query, rec.Body)
			}
		}
	}
}
