package notifyemail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"time"
)

type Address struct {
	Address string `json:"address"`
	Name    string `json:"name"`
}

type Config struct {
	URL  string
	From Address
}

type Client struct {
	config Config
	http   *http.Client
}

func New(config Config) (*Client, error) {
	parsed, err := url.Parse(config.URL)
	address, mailErr := mail.ParseAddress(config.From.Address)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil ||
		parsed.Fragment != "" || mailErr != nil {
		return nil, errors.New("invalid email notification configuration")
	}
	config.From.Address = address.Address
	return &Client{config: config, http: &http.Client{
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (c *Client) Send(ctx context.Context, template string, to Address, fields map[string]string, key string) error {
	address, err := mail.ParseAddress(to.Address)
	if err != nil || address.Address != to.Address || template == "" {
		return errors.New("invalid email notification recipient or template")
	}
	inner, err := json.Marshal(map[string]any{
		"template_key": template, "track_clicks": true, "track_opens": true, "from": c.config.From,
		"to": []map[string]Address{{"email_address": to}}, "merge_info": fields,
	})
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{"type": 3, "content": string(inner)})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config.URL, io.NopCloser(bytes.NewReader(body)))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	request.ContentLength = int64(len(body))
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return errors.New("email notification was not accepted")
	}
	_, err = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	return err
}
