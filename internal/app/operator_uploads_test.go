package app_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/xuri/excelize/v2"
	"open_live_platform/internal/admin"
	"open_live_platform/internal/app"
	"open_live_platform/internal/upload"
)

func adminForm(t *testing.T, a editorialAPI, route, name, typ string, data []byte, fields map[string]string, want int) json.RawMessage {
	t.Helper()
	var buf bytes.Buffer
	form := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := form.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if name != "" {
		header := textproto.MIMEHeader{}
		header.Set("Content-Disposition", fmt.Sprintf("form-data; name=\"file\"; filename=%q", name))
		header.Set("Content-Type", typ)
		part, err := form.CreatePart(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = part.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, route, &buf)
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("X-Loverose-Admin", admin.OperationTenant)
	rec := httptest.NewRecorder()
	a.operator.ServeHTTP(rec, req)
	var result struct {
		Status string
		Data   json.RawMessage
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || rec.Code != want || result.Status != fmt.Sprint(want) {
		t.Fatalf("%s = %d %s", route, rec.Code, rec.Body.String())
	}
	return result.Data
}

func TestPostgresOperatorFileUploads(t *testing.T) {
	today := time.Now().UTC().Format("20060102")
	raw := []byte("\x89PNG\r\n\x1a\nTEST")
	var uploads int
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil || r.URL.Path != "/media/backoffice/"+today+"/image.png" || r.Header.Get("Content-Type") != "image/png" || r.Method != "PUT" || !bytes.Equal(raw, got) || !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256") {
			t.Error("incorrect signed image upload")
			http.Error(w, "bad", 400)
			return
		}
		uploads++
		w.WriteHeader(200)
	}))
	defer storage.Close()
	a := startEditorialAPI(t, func(c *app.Config) {
		c.AdminImagePrefix = "backoffice"
		c.Upload = upload.Config{Bucket: "media", Endpoint: storage.URL, KeyID: "key", Secret: "secret", Parent: "client-assets"}
	})
	key := adminForm(t, a, "/api/operation/platform/upload/images", "image.png", "image/png", raw, nil, 200)
	if string(key) != fmt.Sprintf("%q", "backoffice/"+today+"/image.png") || uploads != 1 {
		t.Fatal(string(key), uploads)
	}
	const word = "/api/operation/system/bad_words/"
	adminForm(t, a, word+"add", "review.txt", "text/plain", []byte("sample"), map[string]string{"description": "test", "forCountry": "JP", "forScience": "chat", "status": "1"}, 200)
	var page struct {
		List  []map[string]any
		Total int
	}
	a.call(t, word+"page", map[string]int{}, 200, &page)
	if page.Total != 1 || page.List[0]["name"] != "review.txt" || page.List[0]["file"] != nil {
		t.Fatal(page)
	}
	id := page.List[0]["id"].(string)
	adminForm(t, a, word+"edit", "", "", nil, map[string]string{"id": id, "description": "changed", "status": "0"}, 200)
	a.call(t, word+"page", map[string]int{}, 200, &page)
	if page.List[0]["description"] != "changed" || page.List[0]["name"] != "review.txt" {
		t.Fatal(page)
	}
	a.call(t, word+"delete", map[string]string{"id": id}, 200, nil)
	a.call(t, word+"page", map[string]int{}, 200, &page)
	if page.Total != 0 {
		t.Fatal(page)
	}
}

func TestPostgresGiftWorkbook(t *testing.T) {
	a := startEditorialAPI(t)
	makeBook := func(rows ...[]any) []byte {
		t.Helper()
		book := excelize.NewFile()
		defer book.Close()
		for i, row := range rows {
			if err := book.SetSheetRow("Sheet1", fmt.Sprintf("A%d", i+1), &row); err != nil {
				t.Fatal(err)
			}
		}
		buf, err := book.WriteToBuffer()
		if err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	const sheet = "/api/operation/platform/upload/config_file"
	const mime = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	header := []any{"id", "type", "name", "price", "icon"}
	row := []any{"90123456789012345", 1, "Gift", 15, "coin.png"}
	invalid := makeBook(header, row, []any{"bad-id", 1, "Other", 20})
	adminForm(t, a, sheet, "gift_config.xlsx", mime, invalid, nil, 400)
	good := makeBook(header, row)
	adminForm(t, a, sheet, "gift_config.xlsx", mime, good, nil, 200)
	var detail map[string]any
	a.call(t, "/api/operation/gifts/gift/detail", map[string]string{"id": "90123456789012345"}, 200, &detail)
	if detail["name"] != "Gift" || detail["price"] != "15" {
		t.Fatal(detail)
	}
	adminForm(t, a, "/api/operation/gifts/gift/upload_file", "gift_config.xlsx", mime, good, nil, 409)
}
