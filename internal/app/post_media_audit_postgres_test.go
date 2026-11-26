package app_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"open_live_platform/internal/app"
	"open_live_platform/internal/liveaudit"
)

func TestPostgresShumeiMixedMediaManualEscalation(t *testing.T) {
	config, db := postgresTestDatabase(t)
	var machine, manual atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Access, Application, Event string
			AccessKey                  string `json:"accessKey"`
			AppID                      string `json:"appId"`
			EventID                    string `json:"eventId"`
			Callback                   string
			Data                       struct {
				Channel, TokenID string
				Contents         json.RawMessage
				PassThrough      struct {
					UserID, DataID string
					RiskInfo       map[string]string
				}
			}
			PassThrough struct{ UserID, DataID string }
		}
		if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&request) != nil || request.AccessKey != "media-key" || request.AppID != "media-app" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/media":
			var content []struct{ DataType, Content, TxtType, ImgType, AudioType string }
			if json.Unmarshal(request.Data.Contents, &content) != nil || len(content) != 3 || content[0].DataType != "text" ||
				content[0].Content != "A mixed-media post" || content[0].TxtType != "TEXTRISK" || content[1].DataType != "image" || content[1].ImgType == "" ||
				content[2].DataType != "video" || content[2].AudioType == "" || request.EventID != "post" ||
				request.Callback != "http://audit.local/sm/media/callback" || request.PassThrough.UserID != request.Data.TokenID || request.PassThrough.DataID == "" {
				t.Errorf("incorrect Shumei mixed-media protocol: %s", request.Data.Contents)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			machine.Add(1)
			fmt.Fprint(w, `{"code":1100,"requestId":"media-1"}`)
		case "/manual":
			var content struct{ Texts, Images, Videos []string }
			if json.Unmarshal(request.Data.Contents, &content) != nil || len(content.Texts) != 1 || len(content.Images) != 1 || len(content.Videos) != 1 ||
				request.Data.Channel != "post" || request.Data.PassThrough.UserID != request.Data.TokenID || request.Data.PassThrough.DataID == "" ||
				request.Data.PassThrough.RiskInfo["riskImageDesc"] != "requires human inspection" {
				t.Errorf("incorrect Shumei manual escalation protocol: %s", request.Data.Contents)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			manual.Add(1)
			fmt.Fprint(w, `{"code":1100,"requestId":"manual-1"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	config.PostReviewToken = "mixed-review-token"
	config.LiveAudit = liveaudit.Config{AccessKey: "media-key", AppID: "media-app", CallbackBase: "http://audit.local",
		MediaURL: server.URL + "/media", ManualMediaURL: server.URL + "/manual"}
	run := func() *app.App {
		t.Helper()
		instance, err := app.New(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(instance.Close)
		return instance
	}
	first, second := run(), run()
	owner := subscriptionTestUser(t, first, "mixed-owner", true)
	outsider := subscriptionTestUser(t, second, "mixed-outsider", false)
	created := anchorPostRequest(t, first, "POST", "/anchor/api/create_post", owner.Token, "", map[string]any{"content": "A mixed-media post", "level": 0,
		"url": []map[string]string{{"url": "https://media.invalid/preview.jpg"}, {"url": "https://media.invalid/clip.mp4"}}}, http.StatusOK)
	id := strconv.FormatInt(int64(created["result"].(map[string]any)["post_id"].(float64)), 10)
	const get = "/anchor/api/get_one_post/"
	if out := anchorPostRequest(t, second, "GET", get+id, outsider.Token, "", nil, 200); out["data"] != nil {
		t.Fatalf("pending mixed-media post leaked: %+v", out)
	}
	message := map[string]any{"requestId": "media-1", "riskLevel": "REVIEW",
		"passThrough": map[string]string{"userId": strconv.FormatInt(owner.User.ID, 10), "dataId": id},
		"details":     map[string]any{"images": []map[string]string{{"riskDescription": "requires human inspection"}}}}
	anchorPostRequest(t, second, "POST", "/sm/media/callback", config.PostReviewToken, "", message, 200)
	anchorPostRequest(t, first, "POST", "/sm/media/callback", config.PostReviewToken, "", message, 200)
	message["requestId"], message["riskLevel"] = "untracked", "PASS"
	anchorPostRequest(t, second, "POST", "/sm/media_manual/callback", config.PostReviewToken, "", message, 404)
	message["requestId"] = "manual-1"
	anchorPostRequest(t, first, "POST", "/sm/media_manual/callback", config.PostReviewToken, "", message, 200)
	anchorPostRequest(t, second, "POST", "/sm/media_manual/callback", config.PostReviewToken, "", message, 200)
	if out := anchorPostRequest(t, second, "GET", get+id, outsider.Token, "", nil, 200); out["status"] != "200" || out["data"] == nil {
		t.Fatalf("approved mixed-media post not published: %+v", out)
	}
	var history int
	if err := db.QueryRow(t.Context(), `SELECT count(*) FROM post_media_audit_history WHERE post_id=$1::text::bigint`, id).Scan(&history); err != nil {
		t.Fatal(err)
	}
	if history != 2 || manual.Load() != 1 || machine.Load() != 1 {
		t.Fatalf("mixed media history=%d machine submissions=%d manual submissions=%d", history, machine.Load(), manual.Load())
	}
}
