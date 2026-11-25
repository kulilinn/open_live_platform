package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const PathPrefix = "/internal/rpc/"

// Error is the JSON error returned by an RPC endpoint.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message)
}

type Server struct {
	mux *http.ServeMux
}

func NewServer() *Server {
	return &Server{mux: http.NewServeMux()}
}

// Handle registers a JSON POST endpoint under PathPrefix.
func Handle[Request, Response any](s *Server, service string, fn func(context.Context, Request) (Response, error)) {
	s.mux.HandleFunc("POST "+servicePath(service), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var request Request
		if err := decode(r.Body, &request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON request")
			return
		}
		response, err := fn(r.Context(), request)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		body, err := json.Marshal(response)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "invalid JSON response")
			return
		}
		w.Write(body)
	})
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func writeError(w http.ResponseWriter, code int, message string) {
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(Error{Code: code, Message: message})
}

type targets struct {
	urls []string
	next int
}

// Registry rotates independently through each service's target URLs.
type Registry struct {
	mu       sync.Mutex
	services map[string]*targets
}

func NewRegistry() *Registry {
	return &Registry{services: make(map[string]*targets)}
}

// Add appends base URLs for a service in round-robin order.
func (r *Registry) Add(service string, baseURLs ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	set := r.services[service]
	if set == nil {
		set = &targets{}
		r.services[service] = set
	}
	set.urls = append(set.urls, baseURLs...)
}

func (r *Registry) next(service string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	set := r.services[service]
	if set == nil || len(set.urls) == 0 {
		return "", fmt.Errorf("rpc service %q has no targets", service)
	}
	target := set.urls[set.next]
	set.next = (set.next + 1) % len(set.urls)
	return target, nil
}

type Client struct {
	registry *Registry
	http     *http.Client
}

func NewClient(registry *Registry) *Client {
	return &Client{registry: registry, http: &http.Client{Timeout: 10 * time.Second}}
}

// Call sends request to the next target for service and decodes its JSON into response.
func (c *Client) Call(ctx context.Context, service string, request, response any) error {
	body, err := json.Marshal(request)
	if err != nil {
		return err
	}
	target, err := c.registry.next(service)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(target, "/")+servicePath(service), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		rpcErr := &Error{Code: resp.StatusCode, Message: http.StatusText(resp.StatusCode)}
		decode(resp.Body, rpcErr)
		return rpcErr
	}
	if response == nil {
		_, err := io.Copy(io.Discard, resp.Body)
		return err
	}
	return decode(resp.Body, response)
}

func servicePath(service string) string {
	return PathPrefix + url.PathEscape(service)
}

func decode(r io.Reader, value any) error {
	body, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, value)
}
