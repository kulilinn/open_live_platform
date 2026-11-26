package app_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"open_live_platform/internal/app"
	"open_live_platform/internal/auth"
	"open_live_platform/internal/upload"
)

func TestFileUpload(t *testing.T) {
	for _, mode := range []string{"memory", "postgres"} {
		t.Run(mode, func(t *testing.T) {
			var writes atomic.Int32
			var expectedPath string
			first := strings.Repeat("a", 256000)
			tail := "def"
			s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				content, err := io.ReadAll(r.Body)
				if err != nil || r.Method != "PUT" || r.URL.Path != expectedPath || string(content) != first+tail ||
					!strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=key/") ||
					r.Header.Get("X-Amz-Content-Sha256") != fmt.Sprintf("%x", sha256.Sum256(content)) {
					t.Errorf("unexpected S3 PUT: %s %s (%d bytes)", r.Method, r.URL.Path, len(content))
				}
				if writes.Add(1) == 1 {
					w.WriteHeader(503)
					return
				}
				w.WriteHeader(200)
			}))
			defer s3.Close()
			cfg := app.Config{}
			if mode == "postgres" {
				cfg, _ = postgresTestDatabase(t)
			}
			cfg.Upload = upload.Config{Bucket: "images", Endpoint: s3.URL, KeyID: "key", Secret: "secret", Parent: "test", BaseURL: "https://static.example", MaxImage: 512000}
			open := func() *app.App {
				a, err := app.New(t.Context(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(a.Close)
				return a
			}
			a := open()
			b := a
			if mode == "postgres" {
				b = open()
			}
			sign := func(email string) auth.Session {
				s, err := a.Accounts.SignUp(t.Context(), email, "test", "User")
				if err != nil {
					t.Fatal(err)
				}
				return s
			}
			u, other := sign("a@upload.test"), sign("b@upload.test")
			guid := httptest.NewRequest("GET", "/api/guid?fileSuffix=png", nil)
			guid.Header.Set("Authorization", "Bearer "+u.Token)
			w := httptest.NewRecorder()
			a.Server.Handler.ServeHTTP(w, guid)
			var issued struct {
				Data      int64
				UploadID  string
				ChunkSize int64
			}
			if err := json.Unmarshal(w.Body.Bytes(), &issued); err != nil || issued.Data < 1 || issued.UploadID == "" || issued.ChunkSize != 256000 {
				t.Fatalf("guid %s", w.Body)
			}
			name := strconv.FormatInt(issued.Data, 10) + ".png"
			expectedPath = fmt.Sprintf("/images/test/assets/files/%d/%s", u.User.ID, name)
			post := func(server *app.App, who auth.Session, index, count int, chunk string, status int) string {
				t.Helper()
				var body bytes.Buffer
				m := multipart.NewWriter(&body)
				for k, v := range map[string]string{"fileid": name, "upload_id": issued.UploadID, "chunk_index": strconv.Itoa(index), "chunk_number": strconv.Itoa(count)} {
					if err := m.WriteField(k, v); err != nil {
						t.Fatal(err)
					}
				}
				f, err := m.CreateFormFile("chunk", "blob")
				if err != nil {
					t.Fatal(err)
				}
				if _, err = io.WriteString(f, chunk); err != nil {
					t.Fatal(err)
				}
				if err = m.Close(); err != nil {
					t.Fatal(err)
				}
				r := httptest.NewRequest("POST", "/api/upload", &body)
				r.Header.Set("Content-Type", m.FormDataContentType())
				r.Header.Set("Authorization", "Bearer "+who.Token)
				w := httptest.NewRecorder()
				server.Server.Handler.ServeHTTP(w, r)
				if w.Code != status {
					t.Fatalf("part %d: HTTP %d, %s", index, w.Code, w.Body)
				}
				return w.Body.String()
			}
			post(a, other, 0, 2, "ABC", 403)
			if x := post(a, u, 0, 3, "abc", 200); !strings.Contains(x, "4053") {
				t.Fatal(x)
			}
			post(a, u, 1, 2, tail, 200)
			post(b, u, 1, 2, "DEF", 409)
			post(b, u, 0, 2, first, 503)
			if x := post(a, u, 0, 2, first, 200); !strings.Contains(x, "https://static.example/assets/files/") {
				t.Fatal(x)
			}
			post(b, u, 1, 2, tail, 200)
			if writes.Load() != 2 {
				t.Fatalf("S3 requests = %d", writes.Load())
			}
		})
	}
}
