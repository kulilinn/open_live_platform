package anchor

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

var ErrKYC = errors.New("identity verification provider unavailable")

type KYCConfig struct{ URL, Token, Secret, Level string }

func KYCFromEnv() KYCConfig {
	return KYCConfig{URL: os.Getenv("SUMSUB_URL"), Token: os.Getenv("SUMSUB_APP_TOKEN"),
		Secret: os.Getenv("SUMSUB_SECRET_KEY"), Level: os.Getenv("SUMSUB_LEVEL")}
}

type KYC struct {
	config KYCConfig
	client *http.Client
}

type KYCToken struct {
	Token  string `json:"token"`
	UserID string `json:"userId"`
	Status *int8  `json:"status"`
}

type applicant struct {
	ID     string          `json:"id"`
	UserID json.RawMessage `json:"externalUserId"`
	Review struct {
		Status string `json:"reviewStatus"`
		Result struct {
			Answer string `json:"reviewAnswer"`
		} `json:"reviewResult"`
	} `json:"review"`
}

func NewKYC(cfg KYCConfig) (*KYC, error) {
	if cfg == (KYCConfig{}) {
		return nil, nil
	}
	target, err := url.Parse(cfg.URL)
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Hostname() == "" ||
		target.RawQuery != "" || target.Fragment != "" || target.User != nil || cfg.Token == "" || cfg.Secret == "" || cfg.Level == "" {
		return nil, errors.New("valid SUMSUB_URL, SUMSUB_APP_TOKEN, SUMSUB_SECRET_KEY and SUMSUB_LEVEL required together")
	}
	return &KYC{config: cfg, client: &http.Client{Timeout: 8 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (k *KYC) Applicant(ctx context.Context, userID int64) (string, error) {
	if k == nil {
		return "", ErrKYC
	}
	key := strconv.FormatInt(userID, 10)
	body, err := json.Marshal(map[string]string{"externalUserId": key})
	if err != nil {
		return "", err
	}
	var result applicant
	code, err := k.signed(ctx, http.MethodPost, "/resources/applicants?levelName="+url.QueryEscape(k.config.Level), body, &result)
	if err != nil {
		return "", err
	}
	if code == http.StatusConflict {
		code, err = k.signed(ctx, http.MethodGet, "/resources/applicants/-;externalUserId="+key+"/one", nil, &result)
		if err != nil {
			return "", err
		}
	}
	if code < 200 || code >= 300 || result.ID == "" || !result.matches(key) {
		return "", ErrKYC
	}
	return result.ID, nil
}

func (k *KYC) Token(ctx context.Context, userID int64) (KYCToken, error) {
	if k == nil {
		return KYCToken{}, ErrKYC
	}
	key := strconv.FormatInt(userID, 10)
	path := "/resources/accessTokens?userId=" + key + "&levelName=" + url.QueryEscape(k.config.Level)
	var token KYCToken
	code, err := k.signed(ctx, http.MethodPost, path, nil, &token)
	if err != nil || code < 200 || code >= 300 || token.Token == "" || token.UserID != key {
		return KYCToken{}, ErrKYC
	}
	return token, nil
}

func (k *KYC) Verified(ctx context.Context, userID int64, id string) (bool, error) {
	if k == nil || id == "" {
		return false, ErrKYC
	}
	var result applicant
	code, err := k.signed(ctx, http.MethodGet, "/resources/applicants/"+url.PathEscape(id)+"/one", nil, &result)
	if err != nil || code != http.StatusOK || result.ID != id || !result.matches(strconv.FormatInt(userID, 10)) {
		return false, ErrKYC
	}
	return result.Review.Status == "completed" && result.Review.Result.Answer == "GREEN", nil
}

func (a applicant) matches(id string) bool {
	actual := string(a.UserID)
	if len(actual) > 0 && actual[0] == '"' {
		return json.Unmarshal(a.UserID, &actual) == nil && actual == id
	}
	return actual == id
}

func (k *KYC) signed(ctx context.Context, method, path string, body []byte, output any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(k.config.URL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return 0, ErrKYC
	}
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	hash := hmac.New(sha256.New, []byte(k.config.Secret))
	hash.Write([]byte(timestamp + method + req.URL.RequestURI()))
	hash.Write(body)
	req.Header.Set("X-App-Token", k.config.Token)
	req.Header.Set("X-App-Access-Ts", timestamp)
	req.Header.Set("X-App-Access-Sig", hex.EncodeToString(hash.Sum(nil)))
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := k.client.Do(req)
	if err != nil {
		return 0, ErrKYC
	}
	defer response.Body.Close()
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(output); err != nil {
			return response.StatusCode, ErrKYC
		}
	}
	return response.StatusCode, nil
}
