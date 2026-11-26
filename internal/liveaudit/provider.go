package liveaudit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"open_live_platform/internal/rtc"
)

const StreamPath = "/sm/video_stream/callback"
const ManualPath = "/sm/live/callback"

var ErrUnknown = errors.New("unknown media audit request")

type Config struct {
	Enabled                                        bool
	StartURL, StopURL, CallbackBase, CallbackToken string
	AccessKey, AppID, EventID                      string
	RulesFile                                      string
	MediaURL, ManualMediaURL                       string
	Rules                                          map[string]Rule
}

type Rule struct {
	Status, Intercept bool
	Subject           string
}

type Provider struct {
	Config
	signer rtc.Signer
	client *http.Client
}

func New(config Config, signer rtc.Signer) (*Provider, error) {
	if !config.Enabled {
		return nil, nil
	}
	if !signer.Configured() || config.AccessKey == "" || config.AppID == "" || config.EventID == "" || config.CallbackToken == "" {
		return nil, errors.New("live media auditing requires Agora, Shumei event credentials and a callback token")
	}
	for _, target := range []string{config.StartURL, config.StopURL, config.CallbackBase} {
		uri, err := url.Parse(target)
		if err != nil || (uri.Scheme != "http" && uri.Scheme != "https") || uri.Host == "" || uri.User != nil || uri.Fragment != "" || uri.RawQuery != "" {
			return nil, errors.New("invalid live audit URL")
		}
	}
	if config.RulesFile != "" {
		raw, err := os.ReadFile(config.RulesFile)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &config.Rules); err != nil {
			return nil, errors.New("invalid live media audit rules")
		}
	}
	return &Provider{Config: config, signer: signer, client: &http.Client{Timeout: 7 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

type Task struct {
	RoomID, LiveID, OwnerID int64
	OwnerName, RequestID    string
	Version                 int64
	Stop                    bool
}

type Identity struct {
	AnchorID   string `json:"anchorId"`
	Channel    string `json:"channel"`
	AnchorName string `json:"anchorName"`
}

type auditResponse struct {
	RequestID string `json:"requestId"`
	Code      int    `json:"code"`
	Detail    struct {
		DuplicateID string `json:"dupRequestId"`
		ErrorCode   int    `json:"errorCode"`
	} `json:"detail"`
}

func (p *Provider) Start(ctx context.Context, task Task, nid uint32) (string, error) {
	token, err := p.signer.ForRoom(task.RoomID, task.OwnerID, nid, false)
	if err != nil {
		return "", err
	}
	callback := strings.TrimRight(p.CallbackBase, "/") + StreamPath
	request := map[string]any{
		"accessKey": p.AccessKey, "eventId": p.EventID, "appId": p.AppID,
		"imgType": "POLITY_EROTIC_VIOLENT_ADVERT_QRCODE_IMGTEXTRISK", "audioType": "POLITICAL_PORN_AD_ABUSE_MOAN",
		"imgCallback": callback, "audioCallback": callback,
		"data": map[string]any{"streamType": "AGORA", "tokenId": task.OwnerName, "anchorName": task.OwnerName, "room": token.Channel,
			"detectFrequency": 15, "returnAllImg": 1, "returnAllText": 1, "returnFinishInfo": 1,
			"agoraParam": map[string]any{"appId": token.AppID, "channel": token.Channel, "uid": nid, "token": token.Token},
			"extra":      map[string]any{"passThrough": Identity{AnchorID: strconv.FormatInt(task.OwnerID, 10), AnchorName: task.OwnerName, Channel: token.Channel}},
		},
	}
	response, err := p.call(ctx, p.StartURL, request)
	if err != nil {
		return "", err
	}
	switch {
	case response.Code == 1100 && response.RequestID != "":
		return response.RequestID, nil
	case response.Detail.ErrorCode == 1001 && response.Detail.DuplicateID != "":
		return response.Detail.DuplicateID, nil
	default:
		return "", errors.New("live audit start rejected")
	}
}

func (p *Provider) Stop(ctx context.Context, requestID string) error {
	response, err := p.call(ctx, p.StopURL, map[string]string{"accessKey": p.AccessKey, "requestId": requestID})
	if err == nil && response.Code != 1100 {
		err = errors.New("live audit stop rejected")
	}
	return err
}

func (p *Provider) call(ctx context.Context, address string, payload any) (auditResponse, error) {
	var result auditResponse
	data, err := json.Marshal(payload)
	if err != nil {
		return result, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, address, bytes.NewReader(data))
	if err != nil {
		return result, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := p.client.Do(req)
	if err != nil {
		return result, errors.New("live audit provider unavailable")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(res.Body, 128<<10)).Decode(&result) != nil {
		return result, errors.New("invalid live audit provider response")
	}
	return result, nil
}

type Evidence struct {
	RiskLevel string `json:"riskLevel"`
	Label1    string `json:"riskLabel1"`
	Label2    string `json:"riskLabel2"`
	Label3    string `json:"riskLabel3"`
}

func (p *Provider) BlockReasons(details ...*Evidence) []string {
	seen := make(map[string]bool)
	var reasons []string
	for _, detail := range details {
		if detail == nil || (detail.RiskLevel != "REVIEW" && detail.RiskLevel != "REJECT") {
			continue
		}
		rule := p.Rules[strings.Join([]string{"groupchat", detail.Label1, detail.Label2, detail.Label3}, ":")]
		if !rule.Status || !rule.Intercept || rule.Subject != "REJECT" {
			continue
		}
		reason := "Illegal_tip_reason_forbidden_other"
		switch detail.Label1 {
		case "politics", "porn":
			reason = "Illegal_tip_reason_" + detail.Label1
		case "violence":
			reason = "Illegal_tip_reason_violence&terrorism"
		case "ad", "ad_law", "qr":
			reason = "Illegal_tip_reason_forbidden_advertising"
		case "abuse":
			reason = "Illegal_tip_reason_forbidden_personal_abuse"
		}
		if !seen[reason] {
			seen[reason] = true
			reasons = append(reasons, reason)
		}
	}
	return reasons
}

type Decision struct {
	ProviderID, EventID, Kind string
	Identity                  Identity
	Operation                 int
	Duration                  int64
	Reasons                   []string
	Retry                     bool
	Payload                   json.RawMessage
}
