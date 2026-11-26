package dm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

type HTTPModerator struct {
	endpoint string
	client   *http.Client
}

func NewHTTPModerator(endpoint string) (*HTTPModerator, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
		return nil, errors.New("invalid DM moderation URL")
	}
	return &HTTPModerator{endpoint: endpoint, client: &http.Client{
		Timeout:       3 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (m *HTTPModerator) ApproveDirectMessage(ctx context.Context, input SendInput) (bool, error) {
	body, err := json.Marshal(input)
	if err != nil {
		return false, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, m.endpoint, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := m.client.Do(request)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false, fmt.Errorf("DM moderation returned HTTP %d", response.StatusCode)
	}
	var result struct{ Pass *bool }
	if err := json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&result); err != nil {
		return false, err
	}
	if result.Pass == nil {
		return false, errors.New("missing DM moderation decision")
	}
	return *result.Pass, nil
}
