package httpapi

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/posts"
	"open_live_platform/internal/transport/httpx"
)

type postCommentView struct {
	ID         string             `json:"id"`
	PostID     string             `json:"post_id"`
	UserID     string             `json:"uid"`
	Content    string             `json:"content"`
	ParentID   *string            `json:"parent_comment_id"`
	Children   []*postCommentView `json:"children"`
	Likes      int64              `json:"likes"`
	Created    time.Time          `json:"create_at"`
	Updated    *time.Time         `json:"update_at"`
	Photo      string             `json:"user_img"`
	Name       string             `json:"user_name"`
	SearchName string             `json:"user_search_name"`
	Liked      int                `json:"liked"`
	Teacher    string             `json:"teacherLabel"`
	Toy        string             `json:"toyInfo"`
}

func (s postAPI) comments(w http.ResponseWriter, r *http.Request, actor int64) {
	id, ok := postQueryID(w, r, "post_id")
	if !ok {
		return
	}
	ctx := r.Context()
	rows, err := s.entries.PostComments(ctx, actor, id)
	if err != nil {
		postError(w, err)
		return
	}
	profiles := make(map[int64]postProfile)
	byID := make(map[int64]*postCommentView, len(rows))
	for _, row := range rows {
		profile, found := profiles[row.AuthorID]
		if !found {
			profile, err = s.profile(ctx, row.AuthorID)
			if err != nil {
				postError(w, err)
				return
			}
			profiles[row.AuthorID] = profile
		}
		entry := &postCommentView{ID: strconv.FormatInt(row.ID, 10), PostID: strconv.FormatInt(row.PostID, 10),
			UserID: strconv.FormatInt(row.AuthorID, 10), Content: row.Content, Children: []*postCommentView{},
			Likes: row.Likes, Created: row.CreatedAt, Photo: profile.Photo, Name: profile.Display,
			SearchName: profile.Search, Teacher: profile.Teacher, Toy: profile.Toy}
		if entry.Toy == "" {
			entry.Toy = "{}"
		}
		if row.Liked {
			entry.Liked = 1
		}
		if row.ParentID != nil {
			parent := strconv.FormatInt(*row.ParentID, 10)
			entry.ParentID = &parent
		}
		byID[row.ID] = entry
	}
	roots := []*postCommentView{}
	for _, row := range rows {
		if row.ParentID == nil {
			roots = append(roots, byID[row.ID])
		} else if parent := byID[*row.ParentID]; parent != nil {
			parent.Children = append(parent.Children, byID[row.ID])
		}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": roots})
}

func (s postAPI) comment(w http.ResponseWriter, r *http.Request, actor int64) {
	var input struct {
		PostID   subscriptionID  `json:"post_id"`
		ParentID *subscriptionID `json:"parent_comment_id"`
		Content  string          `json:"content"`
	}
	if !decodePost(w, r, &input) {
		return
	}
	if input.PostID < 1 || strings.TrimSpace(input.Content) == "" || (input.ParentID != nil && *input.ParentID < 1) {
		postError(w, posts.ErrInvalid)
		return
	}
	ctx := r.Context()
	if err := auth.CheckCommunication(ctx, s.accounts, actor); err != nil {
		postError(w, err)
		return
	}
	id := int64(input.PostID)
	post, err := s.entries.GetPost(ctx, actor, id)
	if err != nil {
		postError(w, err)
		return
	}
	if !post.Full || post.Status != posts.Approved {
		postError(w, posts.ErrForbidden)
		return
	}
	if s.options.Reviewer != nil {
		verdict, err := s.options.Reviewer.Review(ctx, posts.ReviewRequest{Kind: "comment", UserID: actor, PostID: id, Text: input.Content})
		if verdict == posts.Rejected && err == nil {
			httpx.JSON(w, http.StatusOK, map[string]string{"status": "4060"})
			return
		}
		if err != nil || verdict != posts.Approved {
			httpx.Error(w, http.StatusServiceUnavailable, "comment review is unavailable")
			return
		}
	} else if s.options.AuditRequired {
		httpx.Error(w, http.StatusServiceUnavailable, "comment review is unavailable")
		return
	}
	c := posts.Comment{AuthorID: actor, PostID: id, Content: input.Content}
	if input.ParentID != nil {
		parent := int64(*input.ParentID)
		c.ParentID = &parent
	}
	postStatus(w, s.entries.CreatePostComment(ctx, c))
}

func (s postAPI) deleteComment(w http.ResponseWriter, r *http.Request, actor int64) {
	id, ok := postQueryID(w, r, "comment_id")
	if !ok {
		return
	}
	if err := s.entries.DeletePostComment(r.Context(), actor, id); err != nil {
		postError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": nil})
}

func (s postAPI) like(comment bool) userHandler {
	return func(w http.ResponseWriter, r *http.Request, actor int64) {
		var input struct {
			PostID    subscriptionID `json:"post_id"`
			CommentID subscriptionID `json:"comment_id"`
			Liked     *bool          `json:"is_like"`
		}
		if !decodePost(w, r, &input) {
			return
		}
		id, update := input.PostID, s.entries.LikePost
		if comment {
			id, update = input.CommentID, s.entries.LikePostComment
		}
		if id < 1 || input.Liked == nil {
			postError(w, posts.ErrInvalid)
			return
		}
		ctx := r.Context()
		if err := auth.CheckCommunication(ctx, s.accounts, actor); err != nil {
			postError(w, err)
			return
		}
		count, err := update(ctx, actor, int64(id), *input.Liked)
		if err != nil {
			postError(w, err)
			return
		}
		httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": map[string]int64{"count": count}})
	}
}

func (s postAPI) reactions(tips bool) userHandler {
	return func(w http.ResponseWriter, r *http.Request, actor int64) {
		id, ok := postQueryID(w, r, "post_id")
		if !ok {
			return
		}
		page, size, err := postReactionPage(r)
		if err != nil {
			postError(w, err)
			return
		}
		ctx := r.Context()
		result, err := s.entries.PostReactions(ctx, actor, id, tips, (page-1)*size, size)
		if err != nil {
			postError(w, err)
			return
		}
		rows := make([]map[string]any, 0, len(result.Rows))
		for _, reaction := range result.Rows {
			profile, err := s.profile(ctx, reaction.UserID)
			if err != nil {
				postError(w, err)
				return
			}
			rows = append(rows, map[string]any{"user_id": strconv.FormatInt(reaction.UserID, 10), "user_name": profile.Display,
				"user_photo": profile.Photo, "amount": reaction.Points, "create_at": reaction.CreatedAt})
		}
		httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": rows, "page": (result.Total + int64(size) - 1) / int64(size)})
	}
}

func postReactionPage(r *http.Request) (int, int, error) {
	page, size := 1, 20
	query := r.URL.Query()
	if value := query.Get("page"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 || n > 1_000_000 {
			return 0, 0, posts.ErrInvalid
		}
		page = max(1, n)
	}
	if value := query.Get("page_size"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > 100 {
			return 0, 0, posts.ErrInvalid
		}
		size = n
	}
	return page, size, nil
}
