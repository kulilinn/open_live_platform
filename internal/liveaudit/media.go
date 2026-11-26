package liveaudit

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"open_live_platform/internal/posts"
)

type MediaReviewer struct{ provider *Provider }

func NewMediaReviewer(config Config) (*MediaReviewer, error) {
	if config.MediaURL == "" && config.ManualMediaURL == "" {
		return nil, nil
	}
	if config.AccessKey == "" || config.AppID == "" || config.MediaURL == "" || config.CallbackBase == "" {
		return nil, errors.New("mixed media auditing requires Shumei credentials and its callback URL")
	}
	for _, target := range []string{config.MediaURL, config.ManualMediaURL, config.CallbackBase} {
		if target == "" {
			continue
		}
		uri, err := url.Parse(target)
		if err != nil || (uri.Scheme != "http" && uri.Scheme != "https") || uri.Host == "" || uri.User != nil || uri.Fragment != "" || uri.RawQuery != "" {
			return nil, errors.New("invalid mixed media audit URL")
		}
	}
	return &MediaReviewer{provider: &Provider{Config: config, client: &http.Client{Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}}, nil
}

func (r *MediaReviewer) Review(ctx context.Context, req posts.ReviewRequest) (int, error) {
	result, err := r.Evaluate(ctx, req)
	return result.Status, err
}

const mediaImageRisk = "POLITICS_PORN_AD_LOGO_BEHAVIOR_OCR_VIOLENCE"
const mediaAudioRisk = "PORN_AD_POLITICAL_ABUSE_MOAN_ANTHEN_AUDIOPOLITICAL"

func (r *MediaReviewer) Evaluate(ctx context.Context, req posts.ReviewRequest) (posts.Evaluation, error) {
	result := posts.Evaluation{Status: posts.Pending}
	data := make([]map[string]any, 0, len(req.Media)+1)
	if req.Text != "" {
		data = append(data, map[string]any{"dataType": "text", "lang": "auto", "content": req.Text, "btId": rand.Text(), "txtType": "TEXTRISK"})
	}
	images, videos, err := postMediaLinks(req.Media)
	if err != nil {
		return result, err
	}
	for _, media := range images {
		data = append(data, map[string]any{"dataType": "image", "lang": "auto", "content": media, "btId": rand.Text(), "imgType": mediaImageRisk})
	}
	for _, media := range videos {
		data = append(data, map[string]any{"dataType": "video", "lang": "auto", "content": media, "btId": rand.Text(), "imgType": mediaImageRisk, "audioType": mediaAudioRisk})
	}
	user, post := strconv.FormatInt(req.UserID, 10), strconv.FormatInt(req.PostID, 10)
	p := r.provider
	response, err := p.call(ctx, p.MediaURL, map[string]any{
		"accessKey": p.AccessKey, "appId": p.AppID, "eventId": "post",
		"callback":    strings.TrimRight(p.CallbackBase, "/") + "/sm/media/callback",
		"data":        map[string]any{"btId": post, "tokenId": user, "contents": data},
		"passThrough": map[string]any{"userId": user, "dataId": post, "items": []any{}},
	})
	if err != nil {
		return result, err
	}
	if response.Code != 1100 || response.RequestID == "" || len(response.RequestID) > 256 {
		return result, errors.New("mixed media audit submission rejected")
	}
	result.RequestID = response.RequestID
	return result, nil
}

func (r *MediaReviewer) ManualEnabled() bool { return r != nil && r.provider.ManualMediaURL != "" }

func (r *MediaReviewer) SubmitManual(ctx context.Context, post posts.Post, info map[string]string) (string, error) {
	images, videos, err := postMediaLinks(post.Media)
	if err != nil {
		return "", err
	}
	user, id := strconv.FormatInt(post.OwnerID, 10), strconv.FormatInt(post.ID, 10)
	p := r.provider
	response, err := p.call(ctx, p.ManualMediaURL, map[string]any{
		"accessKey": p.AccessKey, "appId": p.AppID,
		"data": map[string]any{"channel": "post", "tokenId": user,
			"contents":    map[string]any{"texts": []string{post.Content}, "images": images, "videos": videos},
			"passThrough": map[string]any{"userId": user, "dataId": id, "items": []any{}, "riskInfo": info},
		},
	})
	if err != nil {
		return "", err
	}
	if response.Code != 1100 || response.RequestID == "" || len(response.RequestID) > 256 {
		return "", errors.New("manual media audit submission rejected")
	}
	return response.RequestID, nil
}

func postMediaLinks(media []posts.Media) (images, videos []string, err error) {
	images, videos = []string{}, []string{}
	for _, file := range media {
		parsed, err := url.Parse(file.URL)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil {
			return nil, nil, errors.New("invalid post media URL")
		}
		switch strings.ToLower(path.Ext(parsed.Path)) {
		case ".jpg", ".jpeg", ".png", ".gif", ".jfif", ".webp":
			images = append(images, file.URL)
		case ".mp4", ".avi", ".mov", ".mkv":
			videos = append(videos, file.URL)
		default:
			return nil, nil, errors.New("unsupported post media kind")
		}
	}
	return images, videos, nil
}
