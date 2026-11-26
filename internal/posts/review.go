package posts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"
)

type ReviewRequest struct {
	Kind   string  `json:"kind"`
	UserID int64   `json:"user_id,string"`
	PostID int64   `json:"post_id,string"`
	Text   string  `json:"text"`
	Media  []Media `json:"media,omitempty"`
}

type Reviewer interface {
	Review(context.Context, ReviewRequest) (int, error)
}

type Evaluation struct {
	Status          int    `json:"pass_status"`
	RequestID       string `json:"request_id"`
	ManualRequestID string `json:"manual_request_id"`
}

type TrackedReviewer interface {
	Evaluate(context.Context, ReviewRequest) (Evaluation, error)
}

type HTTPReviewer struct {
	endpoint, token string
	client          *http.Client
}

func NewHTTPReviewer(endpoint, token string) (Reviewer, error) {
	if endpoint == "" && token == "" {
		return nil, nil
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return nil, errors.New("POST_AUDIT_URL must be an HTTP or HTTPS review endpoint")
	}
	return &HTTPReviewer{endpoint: endpoint, token: token, client: &http.Client{Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (r *HTTPReviewer) Review(ctx context.Context, input ReviewRequest) (int, error) {
	result, err := r.Evaluate(ctx, input)
	return result.Status, err
}

func (r *HTTPReviewer) Evaluate(ctx context.Context, input ReviewRequest) (Evaluation, error) {
	pending := Evaluation{Status: Pending}
	data, err := json.Marshal(input)
	if err != nil {
		return pending, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint, bytes.NewReader(data))
	if err != nil {
		return pending, err
	}
	req.Header.Set("Content-Type", "application/json")
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}
	res, err := r.client.Do(req)
	if err != nil {
		return pending, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return pending, errors.New("post audit provider rejected the request")
	}
	var result Evaluation
	if err := json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&result); err != nil {
		return pending, err
	}
	if result.Status < Pending || result.Status > Approved || len(result.RequestID) > 256 || len(result.ManualRequestID) > 256 {
		return pending, errors.New("invalid post audit provider verdict")
	}
	return result, nil
}
