package posts

import (
	"context"
	"errors"
	"time"
)

var (
	ErrNotFound           = errors.New("post or comment not found")
	ErrForbidden          = errors.New("post action is forbidden")
	ErrDeleted            = errors.New("post has already been deleted")
	ErrInvalid            = errors.New("invalid post or comment")
	ErrReviewConflict     = errors.New("post review has already completed")
	ErrPaymentUnavailable = errors.New("post payment is unavailable")
	ErrPaymentConflict    = errors.New("payment request conflicts with an existing order")
)

const (
	Pending  = 1
	Rejected = 2
	Approved = 3
)

const (
	NoticePublished        = 2
	NoticeLike             = 7
	NoticeComment          = 8
	NoticeTip              = 9
	NoticeMachineRejection = 17
	NoticeManualRejection  = 18
	NoticeCommentRejection = 30
)

type ReviewDecision struct {
	Status int
	Notice int8
}

type Media struct {
	URL      string  `json:"url"`
	Filename string  `json:"filename"`
	ID       string  `json:"id"`
	BlurURL  string  `json:"blur_url"`
	Cover    *string `json:"cover_img"`
	Length   *int    `json:"video_length"`
}

type Input struct {
	Content     string  `json:"content"`
	Level       int     `json:"level"`
	Price       int64   `json:"price"`
	Media       []Media `json:"url"`
	OnlyText    int     `json:"only_text"`
	Images      int     `json:"img_num"`
	Videos      int     `json:"video_num"`
	VideoLength int     `json:"video_length"`
	Reference   string  `json:"reference_post"`
}

type Post struct {
	Input
	AuditRequest, ManualAuditRequest          string
	ID, OwnerID                               int64
	Status                                    int
	CreatedAt                                 time.Time
	Deleted                                   bool
	Full, Bought, Liked                       bool
	LikeCount, CommentCount, TipPoints, MyTip int64
}

func (p Post) Visible(viewer int64) bool {
	return !p.Deleted && (p.OwnerID == viewer || (p.Status == Approved && p.Level != 1))
}

type Filter struct {
	Viewer, Owner, Before int64
	OwnOnly               bool
	Media, Kind, Order    int
	Search                string
	Offset, Size          int
}

type Page[T any] struct {
	Rows  []T
	Total int64
}

type Comment struct {
	ID, PostID, AuthorID int64
	ParentID             *int64
	Content              string
	ManualAuditRequest   string
	CreatedAt            time.Time
	Likes                int64
	Liked, Deleted       bool
}

type Reaction struct {
	UserID    int64
	Points    int64
	CreatedAt time.Time
}

type Stats struct {
	Posts, Images, Videos int64
}

type Store interface {
	CreatePost(ctx context.Context, actor int64, input Input, status int) (Post, error)
	ReviewPost(ctx context.Context, id int64, decision ReviewDecision) error
	BindPostReview(ctx context.Context, id int64, result Evaluation) error
	MatchPostReview(ctx context.Context, id, owner int64, request string, manual bool) (bool, error)
	GetPost(ctx context.Context, actor, id int64) (Post, error)
	ListPosts(ctx context.Context, filter Filter) (Page[Post], error)
	DeletePost(ctx context.Context, actor, id int64) error
	PostStats(ctx context.Context, owner int64) (Stats, error)
	RecentPostAnchors(ctx context.Context, limit int) ([]int64, error)
	PostBuyerCount(ctx context.Context, id int64) (int64, error)
	CreatePostComment(ctx context.Context, comment Comment) error
	PostComments(ctx context.Context, actor, id int64) ([]Comment, error)
	DeletePostComment(ctx context.Context, actor, id int64) error
	RejectPostComment(ctx context.Context, actor, id int64, content, request string) error
	LikePost(ctx context.Context, actor, id int64, liked bool) (int64, error)
	LikePostComment(ctx context.Context, actor, id int64, liked bool) (int64, error)
	PostReactions(ctx context.Context, actor, id int64, tips bool, offset, limit int) (Page[Reaction], error)
}
