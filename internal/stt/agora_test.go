package stt

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"open_live_platform/internal/rtc"
)

func TestLegacyAgoraSTTProtocol(t *testing.T) {
	ctx := t.Context()
	appid := strings.Repeat("a", 32)
	signer, err := rtc.NewSigner(appid, strings.Repeat("b", 32), "43200")
	if err != nil {
		t.Fatal(err)
	}
	const builder = "token+/="
	base := "/v1/projects/" + appid + "/rtsc/speech-to-text"
	languages := []string{"en-US", "ja-JP"}
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, secret, ok := r.BasicAuth()
		if !ok || key != "test-api" || secret != "test-secret" {
			t.Error("Agora request omitted REST Basic Auth")
		}
		switch r.Method + " " + r.URL.Path {
		case "POST " + base + "/builderTokens":
			var input struct{ InstanceID string }
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.InstanceID != "1" {
				t.Errorf("acquire builder token = %+v, %v", input, err)
			}
			json.NewEncoder(w).Encode(map[string]string{"tokenName": builder})
		case "POST " + base + "/tasks":
			var input struct {
				Languages       []string
				MaxIdleTime     int
				RTCConfig       agoraRTC
				TranslateConfig translationConfig
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil || !reflect.DeepEqual(input.Languages, languages) ||
				input.MaxIdleTime != 360 || input.RTCConfig.ChannelName != "1" || input.RTCConfig.SubBotUID != "102" || input.RTCConfig.PubBotUID != "101" ||
				!strings.HasPrefix(input.RTCConfig.SubBotToken, "007") || !strings.HasPrefix(input.RTCConfig.PubBotToken, "007") ||
				input.TranslateConfig.ForceTranslateInterval != 5 || len(input.TranslateConfig.Languages) != 2 {
				t.Errorf("start legacy STT body unexpected, err = %v", err)
			}
			w.Write([]byte(`{"taskId":"task-one","status":"RUNNING"}`))
		case "GET " + base + "/tasks/task-one":
			w.Write([]byte(`{"taskId":"task-one","status":"RUNNING"}`))
		case "PATCH " + base + "/tasks/task-one":
			if r.URL.Query().Get("updateMask") != "languages,translateConfig.languages" || r.URL.Query().Get("sequenceId") != "7" {
				t.Errorf("unexpected legacy update query: %s", r.URL.RawQuery)
			}
			w.WriteHeader(http.StatusNoContent)
		case "DELETE " + base + "/tasks/task-one":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected Agora operation %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
		if r.URL.Path != base+"/builderTokens" && r.URL.Query().Get("builderToken") != builder {
			t.Error("legacy task request lost its builder token")
		}
	}))
	t.Cleanup(mock.Close)
	agora := newAgora(Config{AppID: appid, APIID: "test-api", APISecret: "test-secret", BaseURL: mock.URL, Version: Legacy}, signer)
	task, err := agora.start(ctx, 1, 2, Legacy, languages)
	if err != nil || task.RoomID != 1 || task.LiveID != 2 || task.RemoteID != "task-one" || task.BuilderToken != builder {
		t.Fatalf("legacy Agora task = %+v, %v", task, err)
	}
	if status, err := agora.status(ctx, task); err != nil || status != "RUNNING" {
		t.Fatalf("legacy task status = %q, %v", status, err)
	}
	task.Sequence = 7
	if err := agora.update(ctx, task, []string{"en-US"}); err != nil {
		t.Fatal(err)
	}
	if err := agora.stop(ctx, task); err != nil {
		t.Fatal(err)
	}
}
