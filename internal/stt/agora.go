package stt

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"open_live_platform/internal/rtc"
)

type Config struct {
	AppID, APIID, APISecret string
	BaseURL, Version        string
	CallbackSecret          string
	HTTPClient              *http.Client
}

func ConfigFromEnv() Config {
	return Config{AppID: os.Getenv("AGORA_APP_ID"), APIID: os.Getenv("AGORA_API_ID"), APISecret: os.Getenv("AGORA_API_SECRET"),
		BaseURL: os.Getenv("AGORA_STT_BASE_URL"), Version: os.Getenv("AGORA_STT_API_VERSION"), CallbackSecret: os.Getenv("AGORA_STT_CALLBACK_SECRET")}
}

const (
	Legacy        = "v1"
	Agent         = "v2"
	pubBot uint32 = 101
	subBot uint32 = 102
)

var (
	ErrUnavailable = errors.New("Agora STT is unconfigured")
	ErrAcquire     = errors.New("rtt acquire error")
	ErrMissing     = errors.New("Agora STT task no longer exists")
)

type agora struct {
	config Config
	signer rtc.Signer
	http   *http.Client
}

func newAgora(config Config, signer rtc.Signer) *agora {
	config.BaseURL = strings.TrimRight(cmp.Or(config.BaseURL, "https://api.agora.io"), "/")
	config.Version = cmp.Or(config.Version, Agent)
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}
	return &agora{config: config, signer: signer, http: client}
}

func (s *agora) enabled() bool {
	return s.config.AppID != "" && s.config.APIID != "" && s.config.APISecret != ""
}

type translationLanguage struct {
	Source string   `json:"source"`
	Target []string `json:"target"`
}

type translationConfig struct {
	Enable                 *bool                 `json:"enable,omitempty"`
	ForceTranslateInterval int                   `json:"forceTranslateInterval,omitempty"`
	Languages              []translationLanguage `json:"languages,omitempty"`
}

func translations(version string, languages []string, updating bool) *translationConfig {
	if len(languages) < 2 {
		if updating && version == Agent {
			disabled := false
			return &translationConfig{Enable: &disabled}
		}
		return nil
	}
	config := &translationConfig{Languages: []translationLanguage{
		{Source: languages[0], Target: []string{languages[1]}},
		{Source: languages[1], Target: []string{languages[0]}},
	}}
	if version == Legacy {
		config.ForceTranslateInterval = 5
	} else {
		enabled := true
		config.Enable = &enabled
	}
	return config
}

type agoraRTC struct {
	ChannelName string `json:"channelName"`
	SubBotUID   string `json:"subBotUid,omitempty"`
	SubBotToken string `json:"subBotToken,omitempty"`
	PubBotUID   string `json:"pubBotUid"`
	PubBotToken string `json:"pubBotToken"`
}

type result struct {
	TaskID  string `json:"taskId"`
	AgentID string `json:"agent_id"`
	Status  string `json:"status"`
}

func (s *agora) start(ctx context.Context, roomID, liveID int64, version string, languages []string) (Task, error) {
	if !s.enabled() {
		return Task{}, ErrUnavailable
	}
	channel := strconv.FormatInt(roomID, 10)
	pub, err := s.signer.ForRoom(roomID, roomID, pubBot, version == Agent)
	if err != nil || pub.Token == "" {
		return Task{}, errors.New("could not sign Agora STT publisher token")
	}
	rtcConfig := agoraRTC{ChannelName: channel, PubBotUID: pub.NID, PubBotToken: pub.Token}
	task := Task{RoomID: roomID, LiveID: liveID, Version: version}
	endpoint := s.project(version) + "/join"
	name := fmt.Sprintf("live-%d-%d", roomID, liveID)
	if version == Legacy {
		sub, err := s.signer.ForRoom(roomID, roomID, subBot, false)
		if err != nil || sub.Token == "" {
			return Task{}, errors.New("could not sign Agora STT subscriber token")
		}
		rtcConfig.SubBotUID, rtcConfig.SubBotToken = sub.NID, sub.Token
		var builder struct {
			TokenName string `json:"tokenName"`
		}
		if err := s.request(ctx, http.MethodPost, s.project(version)+"/builderTokens", map[string]string{"instanceId": channel}, &builder); err != nil || builder.TokenName == "" {
			return Task{}, ErrAcquire
		}
		task.BuilderToken = builder.TokenName
		endpoint = s.taskURL(task)
		name = ""
	}
	var response result
	err = s.request(ctx, http.MethodPost, endpoint, struct {
		Name            string             `json:"name,omitempty"`
		Languages       []string           `json:"languages"`
		MaxIdleTime     int                `json:"maxIdleTime"`
		RTCConfig       agoraRTC           `json:"rtcConfig"`
		TranslateConfig *translationConfig `json:"translateConfig,omitempty"`
	}{name, languages, 360, rtcConfig, translations(version, languages, false)}, &response)
	if err != nil {
		return Task{}, err
	}
	task.RemoteID = cmp.Or(response.AgentID, response.TaskID)
	if task.RemoteID == "" || response.Status == "FAILED" || response.Status == "STOPPED" {
		return Task{}, errors.New("Agora STT returned no running task")
	}
	return task, nil
}

func (s *agora) status(ctx context.Context, task Task) (string, error) {
	var response result
	err := s.request(ctx, http.MethodGet, s.taskURL(task), nil, &response)
	if errors.Is(err, ErrMissing) {
		return "STOPPED", nil
	}
	if err == nil && response.Status == "" {
		return "", errors.New("Agora STT response omitted status")
	}
	return response.Status, err
}

func (s *agora) update(ctx context.Context, task Task, languages []string) error {
	target := s.taskURL(task)
	method := http.MethodPost
	mask := "languages,translateConfig.enable,translateConfig.languages"
	if task.Version == Legacy {
		method = http.MethodPatch
		mask = "languages,translateConfig.languages"
	} else {
		target += "/update"
	}
	endpoint, err := url.Parse(target)
	if err != nil {
		return err
	}
	query := endpoint.Query()
	query.Set("sequenceId", strconv.FormatInt(task.Sequence, 10))
	query.Set("updateMask", mask)
	endpoint.RawQuery = query.Encode()
	return s.request(ctx, method, endpoint.String(), struct {
		Languages       []string           `json:"languages"`
		TranslateConfig *translationConfig `json:"translateConfig,omitempty"`
	}{languages, translations(task.Version, languages, true)}, nil)
}

func (s *agora) stop(ctx context.Context, task Task) error {
	endpoint := s.taskURL(task)
	method := http.MethodDelete
	if task.Version != Legacy {
		endpoint += "/leave"
		method = http.MethodPost
	}
	err := s.request(ctx, method, endpoint, nil, nil)
	if errors.Is(err, ErrMissing) {
		return nil
	}
	return err
}

func (s *agora) project(version string) string {
	path := "/api/speech-to-text/v1/projects/" + url.PathEscape(s.config.AppID)
	if version == Legacy {
		path = "/v1/projects/" + url.PathEscape(s.config.AppID) + "/rtsc/speech-to-text"
	}
	return s.config.BaseURL + path
}

func (s *agora) taskURL(task Task) string {
	if task.Version == Legacy {
		path := s.project(task.Version) + "/tasks"
		if task.RemoteID != "" {
			path += "/" + url.PathEscape(task.RemoteID)
		}
		return path + "?" + url.Values{"builderToken": {task.BuilderToken}}.Encode()
	}
	return s.project(task.Version) + "/agents/" + url.PathEscape(task.RemoteID)
}

func (s *agora) request(ctx context.Context, method, endpoint string, body, output any) error {
	if !s.enabled() {
		return ErrUnavailable
	}
	var data []byte
	if body != nil {
		marshaled, err := json.Marshal(body)
		if err != nil {
			return err
		}
		data = marshaled
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(data))
	if err != nil {
		return errors.New("invalid Agora STT endpoint")
	}
	request.Header.Set("Content-Type", "application/json")
	request.SetBasicAuth(s.config.APIID, s.config.APISecret)
	response, err := s.http.Do(request)
	if err != nil {
		// Legacy task URLs contain builder credentials in their query string.
		return errors.New("Agora STT request failed")
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return ErrMissing
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Agora STT returned HTTP %d", response.StatusCode)
	}
	if output != nil {
		return json.NewDecoder(response.Body).Decode(output)
	}
	return nil
}
