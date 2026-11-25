package httpapi_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/httpapi"
	"open_live_platform/internal/live"
)

func TestRoomChatHistoryHTTP(t *testing.T) {
	accounts := auth.NewStore()
	owner, err := accounts.SignUp(t.Context(), "host@example.org", "secret", "Host")
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := accounts.SignUp(t.Context(), "viewer@example.org", "secret", "Viewer Account")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	httpapi.Register(mux, accounts, live.NewStore())
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	const base = "/api/live/room/"
	roomID, viewerID := itoa(owner.User.ID), itoa(viewer.User.ID)
	post := func(path, token string, body any, want int) {
		t.Helper()
		res := requestJSON(t, server.Client(), http.MethodPost, server.URL+base+path, token, body)
		if res.Code != want {
			t.Fatalf("POST %s = %d, want %d: %v", path, res.Code, want, res.Body)
		}
	}
	postRaw := func(path, token, body string, result any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, base+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST %s = %d: %s", path, rec.Code, rec.Body)
		}
		if err := json.Unmarshal(rec.Body.Bytes(), result); err != nil {
			t.Fatalf("POST %s returned %s: %v", path, rec.Body, err)
		}
	}

	post("start", owner.Token, map[string]string{"title": "Live"}, http.StatusOK)
	post("join/"+roomID, viewer.Token, nil, http.StatusNoContent)
	const messageID = 78591199061446657
	const timestamp = "1760000000"
	rustChat := fmt.Sprintf(`{
		"id":"%d","room":%q,"channel":"chat","priority":0,"sender":%q,"timestamp":%q,
		"message":{"msgType":"C","msgGroup":"chat","message":"你好",
			"userInfo":{"uid":%q,"avatar":"","name":"观众昵称","role":2,"user_level":0,"fans_level":0,
				"isGuardExpired":true,"planName":"","transLang":"en","transList":[]},
			"extension":{"counter":9007199254740993}}
	}`, messageID, roomID, roomID, timestamp, viewerID)
	var ack struct {
		Status string `json:"status"`
		Data   bool   `json:"data"`
		MsgID  int64  `json:"msgId"`
	}
	postRaw("chat", viewer.Token, rustChat, &ack)
	if ack.Status != "200" || !ack.Data || ack.MsgID != messageID {
		t.Fatalf("chat acknowledgement = %+v", ack)
	}
	postRaw("chat", viewer.Token, strings.Replace(rustChat, `"msgType":"C","msgGroup":"chat"`, `"msgGroup":"chat", "msgType":"C"`, 1), &ack)
	if ack.Data || ack.MsgID != messageID {
		t.Fatalf("duplicate chat acknowledgement = %+v", ack)
	}
	post("chat", viewer.Token, map[string]any{"room_id": owner.User.ID, "text": "hello from Go", "id": 17}, http.StatusNoContent)

	type payload struct {
		ID        string `json:"id"`
		Channel   string `json:"channel"`
		Room      string `json:"room"`
		Sender    string `json:"sender"`
		Timestamp string `json:"timestamp"`
		Message   struct {
			Type     string `json:"msgType"`
			Group    string `json:"msgGroup"`
			Text     string `json:"message"`
			UserInfo struct {
				UID  string `json:"uid"`
				Name string `json:"name"`
			} `json:"userInfo"`
			Extension struct {
				Counter json.RawMessage `json:"counter"`
			} `json:"extension"`
		} `json:"message"`
	}
	history := func(token, body string, count int) []payload {
		t.Helper()
		var result struct {
			Status string `json:"status"`
			Data   []struct {
				Content string    `json:"content"`
				Queue   string    `json:"queue"`
				Payload []payload `json:"payload"`
			} `json:"data"`
		}
		postRaw("messages", token, body, &result)
		if result.Status != "200" || result.Data == nil || len(result.Data) != count {
			t.Fatalf("history(%s) = %+v, want %d batches", body, result, count)
		}
		messages := make([]payload, 0, count)
		for _, batch := range result.Data {
			if batch.Content != "live_chat" || batch.Queue != roomID || len(batch.Payload) != 1 {
				t.Fatalf("history batch = %+v", batch)
			}
			message := batch.Payload[0]
			if message.Channel != "chat" || message.Room != roomID || message.Sender != viewerID ||
				message.Message.Type != "C" || message.Message.Group != "chat" || message.Message.UserInfo.UID != viewerID {
				t.Fatalf("history payload = %+v", message)
			}
			messages = append(messages, message)
		}
		return messages
	}
	query := fmt.Sprintf(`{"id":%q}`, roomID)
	messages := history(owner.Token, query, 2)
	if m := messages[0]; m.Message.Text != "hello from Go" || m.Message.UserInfo.Name != viewer.User.Account {
		t.Fatalf("Go chat history = %+v", m)
	}
	if m := messages[1]; m.ID != itoa(messageID) || m.Timestamp != timestamp || m.Message.Text != "你好" ||
		m.Message.UserInfo.Name != "观众昵称" || string(m.Message.Extension.Counter) != "9007199254740993" {
		t.Fatalf("Rust chat history = %+v", m)
	}
	for page, want := range messages {
		body := fmt.Sprintf(`{"id":%q,"page":%d,"size":1}`, roomID, page)
		if message := history(viewer.Token, body, 1)[0]; message.ID != want.ID {
			t.Fatalf("history page %d = %+v, want message %s", page, message, want.ID)
		}
	}

	post("messages", "", json.RawMessage(query), http.StatusUnauthorized)
	post("leave/"+roomID, viewer.Token, nil, http.StatusNoContent)
	history(viewer.Token, query, 0)
	post("join/"+roomID, viewer.Token, nil, http.StatusNoContent)
	post("kicked/"+roomID+"/"+viewerID, owner.Token, nil, http.StatusNoContent)
	history(viewer.Token, query, 0)
	post("leave/"+roomID, owner.Token, nil, http.StatusNoContent)
	post("start", owner.Token, map[string]string{"title": "New broadcast"}, http.StatusOK)
	history(owner.Token, query, 0)
}
