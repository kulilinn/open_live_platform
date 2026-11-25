package rpc

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientRotatesEachService(t *testing.T) {
	start := func(name string) *httptest.Server {
		server := NewServer()
		for _, service := range []string{"live", "control"} {
			Handle(server, service, func(_ context.Context, message string) (string, error) {
				return name + " " + message, nil
			})
		}
		target := httptest.NewServer(server)
		t.Cleanup(target.Close)
		return target
	}
	first, second := start("first"), start("second")
	registry := NewRegistry()
	registry.Add("live", first.URL, second.URL)
	registry.Add("control", second.URL, first.URL)
	client := NewClient(registry)

	for _, call := range []struct {
		service string
		want    string
	}{
		{"live", "first ping"},
		{"control", "second ping"},
		{"live", "second ping"},
		{"control", "first ping"},
		{"live", "first ping"},
	} {
		var got string
		if err := client.Call(t.Context(), call.service, "ping", &got); err != nil {
			t.Fatal(err)
		}
		if got != call.want {
			t.Errorf("%s response = %q, want %q", call.service, got, call.want)
		}
	}
}

func TestRPCError(t *testing.T) {
	server := NewServer()
	Handle(server, "live", func(_ context.Context, _ string) (string, error) {
		return "", errors.New("room closed")
	})
	target := httptest.NewServer(server)
	t.Cleanup(target.Close)
	registry := NewRegistry()
	registry.Add("live", target.URL)
	client := NewClient(registry)

	err := client.Call(t.Context(), "live", "ping", nil)
	var rpcErr *Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != http.StatusInternalServerError || rpcErr.Message != "room closed" {
		t.Errorf("Call error = %v, want RPC error with room closed", err)
	}
	if err := client.Call(t.Context(), "unknown", "ping", nil); err == nil {
		t.Error("Call succeeded for a service without targets")
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, PathPrefix+"live", strings.NewReader(`"ping" garbage`))
	server.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("malformed JSON response = %d, want %d", w.Code, http.StatusBadRequest)
	}
}
