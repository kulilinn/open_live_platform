package app_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"open_live_platform/internal/auth"
)

func openPrivateH5(t *testing.T, server *httptest.Server, session auth.Session) *websocket.Conn {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), "GET", server.URL+"/api/device/qrcode", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+session.Token)
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var ticket struct{ URL string }
	if err := json.NewDecoder(resp.Body).Decode(&ticket); err != nil {
		t.Fatal(err)
	}
	socket, _, err := (&websocket.Dialer{Subprotocols: []string{"h5relayer"}}).DialContext(t.Context(), ticket.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	socket.SetReadDeadline(time.Now().Add(time.Second * 4))
	if _, _, err := socket.ReadMessage(); err != nil {
		socket.Close()
		t.Fatal(err)
	}
	return socket
}
