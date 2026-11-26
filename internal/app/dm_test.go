package app_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"open_live_platform/internal/app"
	"open_live_platform/internal/auth"
	"open_live_platform/internal/dm"
	"open_live_platform/internal/relay"
)

func TestDirectMessages(t *testing.T) {
	testDirectMessages(t, app.Config{}, false)
}

func TestPostgresDirectMessages(t *testing.T) {
	config, _ := postgresTestDatabase(t)
	testDirectMessages(t, config, true)
}

func testDirectMessages(t *testing.T, config app.Config, reopen bool) {
	t.Helper()
	ctx := t.Context()
	var moderationCalls atomic.Int64
	audit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input dm.SendInput
		if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&input) != nil ||
			input.SenderID == 0 || input.ReceiverID == 0 || input.ConversationID == 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		moderationCalls.Add(1)
		if input.Content == "outage" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		json.NewEncoder(w).Encode(map[string]bool{"pass": input.Content != "reject"})
	}))
	t.Cleanup(audit.Close)
	config.DMModerationURL = audit.URL
	config.RelayRust = relay.RustOptions{AppKey: "dm", AppSecret: "local-test"}
	open := func() (*app.App, *httptest.Server) {
		t.Helper()
		platform, err := app.New(ctx, config)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(platform.Close)
		server := httptest.NewServer(platform.Server.Handler)
		t.Cleanup(server.Close)
		return platform, server
	}
	platform, server := open()
	receiverServer := server
	if reopen {
		_, receiverServer = open()
	}
	signUp := func(name string) auth.Session {
		t.Helper()
		session, err := platform.Accounts.SignUp(ctx, name+"@example.test", "secret", name)
		if err != nil {
			t.Fatal(err)
		}
		return session
	}
	fan, anchor, outsider := signUp("fan"), signUp("anchor"), signUp("outsider")
	receiverSocket := openPrivateH5(t, receiverServer, anchor)
	defer receiverSocket.Close()
	fanID, anchorID, outsiderID := strconv.FormatInt(fan.User.ID, 10), strconv.FormatInt(anchor.User.ID, 10), strconv.FormatInt(outsider.User.ID, 10)
	const base = "/api/dm/"
	type response struct {
		Status, Message string
		Data            json.RawMessage
	}
	request := func(method, path string, session auth.Session, body string, wantHTTP int, wantStatus string) response {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, server.URL+base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if session.Token != "" {
			req.Header.Set("Authorization", "Bearer "+session.Token)
		}
		res, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var result response
		if err = json.NewDecoder(res.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != wantHTTP || result.Status != wantStatus {
			t.Fatalf("%s %s = HTTP %d, status %q, message %q", method, path, res.StatusCode, result.Status, result.Message)
		}
		return result
	}
	get := func(path string, session auth.Session) response {
		t.Helper()
		return request(http.MethodGet, path, session, "", http.StatusOK, "200")
	}
	canSend := func(want string) {
		t.Helper()
		if r := get("conversation/can_send/"+anchorID, fan); string(r.Data) != want {
			t.Fatalf("can_send = %s, want %s", r.Data, want)
		}
	}
	request(http.MethodGet, "conversation/get_unread_message_count", auth.Session{}, "", http.StatusUnauthorized, "")
	request(http.MethodGet, "conversation/get_conversation_id/"+anchorID, fan, "", http.StatusOK, "4005")
	canSend("false")
	members := dm.Metadata{
		Fan:    dm.User{ID: fan.User.ID, DisplayName: "fan", SearchName: "fan"},
		Anchor: dm.User{ID: anchor.User.ID, DisplayName: "anchor", SearchName: "host", Photo: "cover.png", Anchor: true},
	}
	id, err := platform.DM.SetDMSubscription(ctx, members, true, "Welcome")
	if err != nil {
		t.Fatal(err)
	}
	conversationID := strconv.FormatInt(id, 10)
	canSend("true")
	if r := get("conversation/get_conversation_id/"+anchorID, fan); string(r.Data) != strconv.Quote(conversationID) {
		t.Fatalf("conversation id = %s", r.Data)
	}
	pageConversations := func(who auth.Session) dm.Page[dm.Conversation] {
		t.Helper()
		data := get(fmt.Sprintf("page_conversation?userId=%d&page=0&pageSize=0", who.User.ID), who).Data
		var page dm.Page[dm.Conversation]
		if err := json.Unmarshal(data, &page); err != nil {
			t.Fatal(err)
		}
		return page
	}
	page := pageConversations(fan)
	if len(page.Data) != 1 || page.Pagination.Page != 1 || page.Pagination.Size != 1 || page.Pagination.Total != 1 ||
		page.Data[0].OtherUser.ID != anchor.User.ID || !page.Data[0].OtherUser.Anchor || page.Data[0].OtherUser.Photo != "cover.png" ||
		page.Data[0].Unread != 1 || page.Data[0].LastMessage != "Welcome" {
		t.Fatalf("first conversation page = %+v", page)
	}
	messages := "page_message?conversationId=" + conversationID + "&page=1&pageSize=10000"
	request(http.MethodGet, messages, outsider, "", http.StatusOK, "4005")
	request(http.MethodGet, "read_message/"+conversationID, outsider, "", http.StatusOK, "4005")
	request(http.MethodGet, "hide_conversation/"+conversationID, outsider, "", http.StatusOK, "4005")
	request(http.MethodGet, "page_conversation?userId="+fanID+"&page=1&pageSize=1", outsider, "", http.StatusUnauthorized, "401")
	post := func(session auth.Session, sender, recipient, content string, httpStatus int, status string) response {
		t.Helper()
		body := fmt.Sprintf(`{"conversationId":%q,"senderId":%q,"receiverId":%q,"messageType":1,"content":%q}`,
			conversationID, sender, recipient, content)
		return request(http.MethodPost, "send_message", session, body, httpStatus, status)
	}
	post(outsider, fanID, anchorID, "spoofed", http.StatusUnauthorized, "401")
	post(outsider, outsiderID, anchorID, "guessing", http.StatusOK, "4005")
	post(fan, fanID, outsiderID, "wrong receiver", http.StatusForbidden, "403")
	if got := moderationCalls.Load(); got != 0 {
		t.Fatalf("moderation called before permission check: %d", got)
	}
	post(fan, fanID, anchorID, "reject", http.StatusOK, "4061")
	post(fan, fanID, anchorID, "outage", http.StatusServiceUnavailable, "503")
	if got := pageConversations(fan).Data[0].LastMessage; got != "Welcome" {
		t.Fatalf("message after failed moderation = %q", got)
	}
	get("hide_conversation/"+conversationID, fan)
	if got := pageConversations(fan); got.Data == nil || len(got.Data) != 0 || got.Pagination.Total != 0 {
		t.Fatalf("hidden conversations = %+v", got)
	}
	if got := pageConversations(anchor); len(got.Data) != 1 {
		t.Fatalf("other participant cannot see conversation = %+v", got)
	}
	sent := post(fan, fanID, anchorID, "Approved", http.StatusOK, "200")
	var message dm.Message
	receiverSocket.SetReadDeadline(time.Now().Add(3 * time.Second))
	var frame struct {
		Content, Queue string
		Payload        struct {
			Message struct{ Content, ReceiverID string }
		}
	}
	for frame.Content != "dm" {
		if err := receiverSocket.ReadJSON(&frame); err != nil {
			t.Fatal(err)
		}
	}
	if frame.Queue != anchorID || frame.Payload.Message.Content != "Approved" || frame.Payload.Message.ReceiverID != anchorID {
		t.Fatalf("private frame = %+v", frame)
	}
	if err := json.Unmarshal(sent.Data, &message); err != nil || message.ID == 0 || message.ConversationID != id ||
		message.Status != dm.Pending || message.ReceiverID != anchor.User.ID || message.Content != "Approved" {
		t.Fatalf("sent message = %+v, err = %v", message, err)
	}
	if got := pageConversations(fan); len(got.Data) != 1 || got.Data[0].LastMessage != "Approved" {
		t.Fatalf("reopened conversation = %+v", got)
	}
	unread := func(who auth.Session, want string) {
		t.Helper()
		if got := get("conversation/get_unread_message_count", who); string(got.Data) != want {
			t.Fatalf("unread count for %s = %s, want %s", who.User.Account, got.Data, want)
		}
	}
	unread(fan, "1")
	unread(anchor, "1")
	get("read_message/"+conversationID, fan)
	unread(fan, "0")
	unread(anchor, "1")
	if _, err := platform.DM.SetDMSubscription(ctx, members, false, ""); err != nil {
		t.Fatal(err)
	}
	canSend("false")
	post(fan, fanID, anchorID, "expired", http.StatusForbidden, "403")
	if restored, err := platform.DM.SetDMSubscription(ctx, members, true, "second welcome"); err != nil || restored != id {
		t.Fatalf("restored conversation = %d, err = %v", restored, err)
	}
	if reopen {
		server.Close()
		platform.Close()
		_, server = open()
	}
	canSend("true")
	var history dm.Page[dm.Message]
	if err := json.Unmarshal(get(messages, anchor).Data, &history); err != nil {
		t.Fatal(err)
	}
	if len(history.Data) != 2 || history.Pagination.Size != 100 || history.Pagination.Total != 2 ||
		history.Data[0].Content != "Approved" || history.Data[0].Status != dm.Pending ||
		history.Data[1].Content != "Welcome" || history.Data[1].Status != dm.Read {
		t.Fatalf("message history = %+v", history)
	}
	if got := moderationCalls.Load(); got != 3 {
		t.Fatalf("moderation called %d times, want 3", got)
	}
}
