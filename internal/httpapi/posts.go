package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/live"
	"open_live_platform/internal/payment"
	"open_live_platform/internal/posts"
	"open_live_platform/internal/subscription"
	"open_live_platform/internal/transport/httpx"
)

type PostOptions struct {
	Reviewer      posts.Reviewer
	AuditRequired bool
	ReviewToken   string
}

type postAPI struct {
	server
	entries       posts.Store
	subscriptions subscription.Store
	billing       *payment.Client
	options       PostOptions
}

func RegisterPosts(mux *http.ServeMux, accounts auth.AccountStore, rooms live.RoomStore, entries posts.Store,
	subscriptions subscription.Store, billing *payment.Client, options PostOptions) {
	s := postAPI{server: server{accounts: accounts, rooms: rooms}, entries: entries, subscriptions: subscriptions, billing: billing, options: options}
	for route, handler := range map[string]userHandler{
		"POST /anchor/api/create_post":           s.createPost,
		"POST /anchor/api/get_post":              s.listPosts,
		"GET /anchor/api/get_one_post/{post_id}": s.getPost,
		"GET /anchor/api/query_post":             s.queryPost,
		"POST /anchor/api/del_post":              s.deletePost,
		"POST /anchor/api/page":                  s.anchorPage,
		"POST /anchor/api/recent_update":         s.recentPosts,
		"POST /anchor/api/search":                s.searchPostAnchors,
		"GET /anchor/api/get_post_comment":       s.comments,
		"POST /anchor/api/comment_post":          s.comment,
		"GET /anchor/api/del_comment":            s.deleteComment,
		"POST /anchor/api/like_post":             s.like(false),
		"POST /anchor/api/like_comment":          s.like(true),
		"GET /anchor/api/get_post_like":          s.reactions(false),
		"GET /anchor/api/get_post_donate_rank":   s.reactions(true),
		"GET /api/sub/pay_post":                  s.payPost,
		"POST /anchor/api/donate_post":           s.donatePost,
	} {
		mux.HandleFunc(route, s.authenticated(handler))
	}
	if options.ReviewToken != "" {
		mux.HandleFunc("POST /internal/anchor-post/review", s.reviewPost)
	}
}

func decodePost(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	return httpx.Decode(w, r, v)
}

func (s postAPI) createPost(w http.ResponseWriter, r *http.Request, actor int64) {
	ctx := r.Context()
	if _, err := (subscriptionAPI{server: s.server}).anchor(ctx, actor); err != nil {
		if errors.Is(err, subscription.ErrForbidden) || errors.Is(err, subscription.ErrNotFound) {
			err = posts.ErrForbidden
		}
		postError(w, err)
		return
	}
	if err := auth.CheckCommunication(ctx, s.accounts, actor); err != nil {
		postError(w, err)
		return
	}
	var input posts.Input
	if !decodePost(w, r, &input) {
		return
	}
	if !validPost(input) {
		postError(w, posts.ErrInvalid)
		return
	}
	if input.Media == nil {
		input.Media = []posts.Media{}
	}
	status := posts.Approved
	if s.options.AuditRequired || s.options.Reviewer != nil {
		status = posts.Pending
	}
	p, err := s.entries.CreatePost(ctx, actor, input, status)
	if err != nil {
		postError(w, err)
		return
	}
	if s.options.Reviewer != nil {
		verdict, err := s.options.Reviewer.Review(ctx, posts.ReviewRequest{Kind: "post", UserID: actor, PostID: p.ID, Text: p.Content, Media: p.Media})
		if err != nil {
			log.Printf("post %d awaiting audit: %v", p.ID, err)
		} else if verdict == posts.Approved || verdict == posts.Rejected {
			if err := s.entries.ReviewPost(ctx, p.ID, verdict); err != nil {
				log.Printf("could not apply post %d audit: %v", p.ID, err)
			}
		}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "result": map[string]int64{"post_id": p.ID}})
}

func validPost(p posts.Input) bool {
	if p.Level < 0 || p.Level > 4 || p.OnlyText < 0 || p.OnlyText > 1 || p.Price < 0 || p.Price > 1<<31-1 ||
		p.Images < 0 || p.Images > 1<<31-1 || p.Videos < 0 || p.Videos > 1<<31-1 || p.VideoLength < 0 || p.VideoLength > 1<<31-1 ||
		(strings.TrimSpace(p.Content) == "" && len(p.Media) == 0) {
		return false
	}
	for _, attachment := range p.Media {
		if !postAssetURL(attachment.URL) || (attachment.BlurURL != "" && !postAssetURL(attachment.BlurURL)) ||
			(attachment.Cover != nil && *attachment.Cover != "" && !postAssetURL(*attachment.Cover)) ||
			(attachment.Length != nil && *attachment.Length < 0) {
			return false
		}
	}
	return true
}

func postAssetURL(value string) bool {
	if value == "" || strings.ContainsAny(value, "\r\n\x00\\") || strings.HasPrefix(value, "//") {
		return false
	}
	u, err := url.Parse(value)
	if err != nil || u.User != nil {
		return false
	}
	if u.Scheme == "" {
		return u.Host == "" && u.Path != ""
	}
	return (strings.EqualFold(u.Scheme, "http") || strings.EqualFold(u.Scheme, "https")) && u.Host != ""
}

func (s postAPI) reviewPost(w http.ResponseWriter, r *http.Request) {
	if !httpx.RequireToken(w, r, s.options.ReviewToken) {
		return
	}
	var input struct {
		ID     subscriptionID `json:"post_id"`
		Status int            `json:"pass_status"`
	}
	if !decodePost(w, r, &input) {
		return
	}
	if input.ID < 1 || (input.Status != posts.Approved && input.Status != posts.Rejected) {
		postError(w, posts.ErrInvalid)
		return
	}
	postStatus(w, s.entries.ReviewPost(r.Context(), int64(input.ID), input.Status))
}

type postProfile struct {
	Name, Photo, Display, Search, Banner, Toy, Teacher string
	Online                                             bool
}

func (s postAPI) profile(ctx context.Context, id int64) (postProfile, error) {
	u, found, err := s.accounts.Find(ctx, id)
	if err != nil || !found {
		return postProfile{}, err
	}
	result := postProfile{Display: u.Account, Name: u.Account, Search: u.Account}
	if p := u.Profile; p != nil {
		result.Name, result.Photo, result.Search, result.Banner, result.Toy = p.Name, p.Photo, p.SearchName, p.Banner, p.ToyInfo
	}
	if room, err := s.rooms.Info(ctx, id); err == nil {
		result.Online = room.Online
		if room.Banner != "" {
			result.Banner = room.Banner
		}
	} else if !errors.Is(err, live.ErrRoomNotFound) {
		return postProfile{}, err
	}
	return result, nil
}

func (s postAPI) viewPost(ctx context.Context, p posts.Post, detail bool) (map[string]any, error) {
	anchor, err := s.profile(ctx, p.OwnerID)
	if err != nil {
		return nil, err
	}
	plans, err := s.subscriptions.Plans(ctx, p.OwnerID, false)
	if err != nil {
		return nil, err
	}
	planViews := []any{}
	for _, plan := range plans {
		if detail {
			planViews = append(planViews, plan)
		} else if p.Level >= subscription.Free && plan.Type == p.Level && len(planViews) == 0 {
			planViews = append(planViews, map[string]any{"id": strconv.FormatInt(plan.ID, 10), "ty": plan.Type, "name": plan.Name, "price": plan.Price, "url": plan.URL})
		}
	}
	content, ref, media, privilege := p.Content, p.Reference, slices.Clone(p.Media), 0
	if media == nil {
		media = []posts.Media{}
	}
	if p.Full {
		privilege = 1
	} else {
		if p.OnlyText == 0 {
			content = "x"
		}
		ref = ""
		for i := range media {
			if media[i].BlurURL == media[i].URL {
				media[i].BlurURL = ""
			}
			media[i].URL, media[i].Cover = "x", nil
		}
	}
	encoded, err := json.Marshal(media)
	if err != nil {
		return nil, err
	}
	state := any(0)
	if detail {
		state = "Offline"
		if anchor.Online {
			state = "Living"
		}
	} else if anchor.Online {
		state = 4
	}
	liked := 0
	if p.Liked {
		liked = 1
	}
	out := map[string]any{"id": strconv.FormatInt(p.ID, 10), "uid": strconv.FormatInt(p.OwnerID, 10), "content": content,
		"level": p.Level, "only_text": p.OnlyText, "price": p.Price, "url": string(encoded), "create_at": p.CreatedAt,
		"privilege": privilege, "media_privilege": 0, "img_num": p.Images, "video_num": p.Videos, "video_length": p.VideoLength,
		"anchor_name": anchor.Name, "anchor_img": anchor.Photo, "anchor_display_name": anchor.Display, "anchor_search_name": anchor.Search,
		"anchor_plans": planViews, "comment_num": p.CommentCount, "like_num": p.LikeCount, "tip_amount": p.TipPoints, "my_tip_amount": p.MyTip,
		"reference_post": ref, "anchor_state": state, "liked": liked, "pass_status": p.Status}
	if !detail {
		out["anchor_info"] = map[string]string{"anchorId": strconv.FormatInt(p.OwnerID, 10), "teacherLabel": anchor.Teacher, "toyInfo": anchor.Toy}
	}
	return out, nil
}

func (s postAPI) listPosts(w http.ResponseWriter, r *http.Request, actor int64) {
	var in struct {
		Owner  subscriptionID `json:"anchor_id"`
		Before subscriptionID `json:"last_post_id"`
		Page   int            `json:"page"`
		Size   int            `json:"page_size"`
		Kind   int            `json:"sort"`
		Order  int            `json:"sort2"`
		Media  int            `json:"filter"`
		Own    *int           `json:"only_self"`
		Search string         `json:"search_content"`
	}
	if !decodePost(w, r, &in) {
		return
	}
	if in.Size < 0 || in.Size > 100 || in.Page < 0 || in.Kind < 0 || in.Kind > 5 || in.Media < 0 || in.Media > 3 ||
		(in.Order != 0 && (in.Order < 6 || in.Order > 9)) {
		postError(w, posts.ErrInvalid)
		return
	}
	if in.Size == 0 {
		in.Size = 20
	}
	if in.Own != nil && in.Owner == 0 {
		in.Owner = subscriptionID(actor)
	}
	offset := 0
	if in.Before == 0 {
		offset = min(max(in.Page-1, 0), int(^uint(0)>>1)/in.Size) * in.Size
	}
	ctx := r.Context()
	page, err := s.entries.ListPosts(ctx, posts.Filter{Viewer: actor, Owner: int64(in.Owner), Before: int64(in.Before), OwnOnly: in.Own != nil,
		Media: in.Media, Kind: in.Kind, Order: in.Order, Search: in.Search, Offset: offset, Size: in.Size})
	if err != nil {
		postError(w, err)
		return
	}
	result := make([]map[string]any, 0, len(page.Rows))
	for _, p := range page.Rows {
		v, err := s.viewPost(ctx, p, false)
		if err != nil {
			postError(w, err)
			return
		}
		result = append(result, v)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": result, "page": page.Total / int64(in.Size)})
}

func (s postAPI) getPost(w http.ResponseWriter, r *http.Request, actor int64) {
	id, ok := httpx.PathID(w, r, "post_id")
	if !ok {
		return
	}
	ctx := r.Context()
	p, err := s.entries.GetPost(ctx, actor, id)
	if errors.Is(err, posts.ErrNotFound) {
		httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": nil, "personal_reward": 0, "teacherLabel": "", "toyInfo": ""})
		return
	}
	if err != nil {
		postError(w, err)
		return
	}
	v, err := s.viewPost(ctx, p, true)
	if err != nil {
		postError(w, err)
		return
	}
	anchor, err := s.profile(ctx, p.OwnerID)
	if err != nil {
		postError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": v, "personal_reward": p.MyTip, "teacherLabel": anchor.Teacher, "toyInfo": anchor.Toy})
}

func (s postAPI) queryPost(w http.ResponseWriter, r *http.Request, actor int64) {
	id, ok := postQueryID(w, r, "post_id")
	if !ok {
		return
	}
	p, err := s.entries.GetPost(r.Context(), actor, id)
	if err != nil {
		postError(w, err)
		return
	}
	if p.OwnerID != actor {
		postError(w, posts.ErrForbidden)
		return
	}
	count, err := s.entries.PostBuyerCount(r.Context(), id)
	if err != nil {
		postError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": map[string]any{"is_pay": (p.Level == 0 && p.Price > 0) || (p.Level != 0 && p.Level != 2), "sub_num": count}})
}

func (s postAPI) deletePost(w http.ResponseWriter, r *http.Request, actor int64) {
	var input struct {
		ID subscriptionID `json:"post_id"`
	}
	if !decodePost(w, r, &input) {
		return
	}
	if input.ID < 1 {
		postError(w, posts.ErrInvalid)
		return
	}
	postStatus(w, s.entries.DeletePost(r.Context(), actor, int64(input.ID)))
}

func postQueryID(w http.ResponseWriter, r *http.Request, key string) (int64, bool) {
	id, err := strconv.ParseInt(r.URL.Query().Get(key), 10, 64)
	if err != nil || id < 1 {
		postError(w, posts.ErrInvalid)
		return 0, false
	}
	return id, true
}

func postStatus(w http.ResponseWriter, err error) {
	if err != nil {
		postError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "200"})
}

func postError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, posts.ErrNotFound):
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "4005", "message": err.Error()})
	case errors.Is(err, posts.ErrDeleted):
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "4080", "message": err.Error()})
	case errors.Is(err, posts.ErrForbidden), errors.Is(err, auth.ErrCommunicationRestricted):
		httpx.Error(w, http.StatusForbidden, err.Error())
	case errors.Is(err, posts.ErrInvalid):
		httpx.Error(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, posts.ErrPaymentConflict), errors.Is(err, posts.ErrReviewConflict):
		httpx.Error(w, http.StatusConflict, err.Error())
	case errors.Is(err, posts.ErrPaymentUnavailable):
		httpx.Error(w, http.StatusServiceUnavailable, err.Error())
	default:
		httpx.Error(w, http.StatusInternalServerError, "post operation failed")
	}
}

func (s postAPI) anchorPage(w http.ResponseWriter, r *http.Request, actor int64) {
	var input struct {
		Owner subscriptionID `json:"anchor_id"`
	}
	if !decodePost(w, r, &input) {
		return
	}
	owner := int64(input.Owner)
	if _, err := (subscriptionAPI{server: s.server}).anchor(r.Context(), owner); err != nil {
		if errors.Is(err, subscription.ErrNotFound) || errors.Is(err, subscription.ErrForbidden) {
			postStatus(w, nil)
		} else {
			postError(w, err)
		}
		return
	}
	ctx := r.Context()
	profile, err := s.profile(ctx, owner)
	if err != nil {
		postError(w, err)
		return
	}
	stats, err := s.entries.PostStats(ctx, owner)
	if err != nil {
		postError(w, err)
		return
	}
	subs, err := s.subscriptions.Relationships(ctx, 0, owner)
	if err != nil {
		postError(w, err)
		return
	}
	fans := map[int64]struct{}{}
	now := time.Now()
	for _, sub := range subs {
		if sub.Active(now) {
			fans[sub.FanID] = struct{}{}
		}
	}
	state := "Offline"
	if profile.Online {
		state = "Living"
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": map[string]any{"img": profile.Photo, "banner": profile.Banner,
		"rank": 10, "post_num": stats.Posts, "video_num": stats.Videos, "sub_num": len(fans), "img_num": stats.Images, "anchor_state": state, "level": 0}})
}

func (s postAPI) recentPosts(w http.ResponseWriter, r *http.Request, _ int64) {
	ctx := r.Context()
	ids, err := s.entries.RecentPostAnchors(ctx, 100)
	if err != nil {
		postError(w, err)
		return
	}
	result := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		p, err := s.profile(ctx, id)
		if err != nil {
			postError(w, err)
			return
		}
		state := 0
		if p.Online {
			state = 4
		}
		result = append(result, map[string]any{"uid": strconv.FormatInt(id, 10), "name": p.Name, "display_name": p.Display,
			"state": state, "img": p.Photo, "banner": p.Banner, "teacherLable": p.Teacher, "toyInfo": p.Toy})
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": result})
}

func (s postAPI) searchPostAnchors(w http.ResponseWriter, r *http.Request, actor int64) {
	var input struct {
		Username *string `json:"username"`
	}
	if !decodePost(w, r, &input) {
		return
	}
	if input.Username == nil {
		postStatus(w, nil)
		return
	}
	ctx := r.Context()
	subs, err := s.subscriptions.Relationships(ctx, actor, 0)
	if err != nil {
		postError(w, err)
		return
	}
	result := []map[string]any{}
	seen := map[int64]struct{}{}
	now := time.Now()
	query := strings.ToLower(*input.Username)
	for _, sub := range subs {
		if _, known := seen[sub.AnchorID]; known || !sub.Active(now) {
			continue
		}
		seen[sub.AnchorID] = struct{}{}
		p, err := s.profile(ctx, sub.AnchorID)
		if err != nil {
			postError(w, err)
			return
		}
		if strings.Contains(strings.ToLower(p.Display), query) || strings.Contains(strings.ToLower(p.Search), query) {
			result = append(result, map[string]any{"uid": strconv.FormatInt(sub.AnchorID, 10), "img": p.Photo, "name": p.Display, "state": 0})
			if len(result) == 100 {
				break
			}
		}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": result})
}
