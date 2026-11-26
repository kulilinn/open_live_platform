package dm

import (
	"context"
	"errors"
	"time"
)

var (
	ErrNotFound  = errors.New("conversation not found")
	ErrForbidden = errors.New("conversation access denied")
)

const (
	Pending   = 1
	Delivered = 2
	Read      = 3
	Welcome   = "chat-sub-welcome"
)

type User struct {
	ID           int64  `json:"uid,string"`
	DisplayName  string `json:"displayName"`
	SearchName   string `json:"searchName"`
	Photo        string `json:"photo"`
	ToyInfo      string `json:"toyInfo"`
	OnlineStatus int32  `json:"onlineStatus"`
	Anchor       bool   `json:"isAnchor"`
	TeacherLabel string `json:"teacherLabel"`
}

type Metadata struct {
	Welcome string `json:"-"`
	Fan     User   `json:"fansInfo"`
	Anchor  User   `json:"anchorInfo"`
}

type Conversation struct {
	ID           int64     `json:"id,string"`
	Type         int8      `json:"conversationType"`
	Members      []string  `json:"members"`
	LastMessage  string    `json:"lastMessage"`
	LastActivity time.Time `json:"lastMessageTime"`
	Metadata     Metadata  `json:"metadata"`
	Deleted      int8      `json:"delFlag"`
	CreatedAt    time.Time `json:"createTime"`
	CanMessage   bool      `json:"canMessage"`
	Unread       int64     `json:"unreadCount"`
	OtherUser    User      `json:"otherUserInfo"`
}

type SendInput struct {
	ConversationID int64  `json:"conversationId,string"`
	SenderID       int64  `json:"senderId,string"`
	ReceiverID     int64  `json:"receiverId,string"`
	Type           int8   `json:"messageType"`
	Content        string `json:"content"`
}

type Message struct {
	ID             int64     `json:"id,string"`
	ConversationID int64     `json:"conversationId,string"`
	SenderID       int64     `json:"senderId,string"`
	ReceiverID     int64     `json:"receiverId,string"`
	GroupID        *int64    `json:"groupId,string"`
	Type           int8      `json:"messageType"`
	Content        string    `json:"content"`
	Status         int8      `json:"status"`
	Deleted        int8      `json:"delFlag"`
	CreatedAt      time.Time `json:"createTime"`
}

type Pagination struct {
	Page       int64 `json:"page"`
	Size       int64 `json:"pageSize"`
	Total      int64 `json:"total"`
	TotalPages int64 `json:"totalPages"`
	HasNext    bool  `json:"hasNext"`
	HasPrev    bool  `json:"hasPrev"`
}

type Page[T any] struct {
	Data       []T        `json:"data"`
	Pagination Pagination `json:"pagination"`
}

type PageRequest struct{ Page, Size int64 }

func NewPageRequest(page, size int64) PageRequest {
	return PageRequest{Page: max(page, 1), Size: max(1, min(100, size))}
}

func (p PageRequest) Window(total int64) (offset, limit int64, details Pagination) {
	p = NewPageRequest(p.Page, p.Size)
	details = Pagination{Page: p.Page, Size: p.Size, Total: total, HasPrev: p.Page > 1}
	if total == 0 {
		return 0, 0, details
	}
	details.TotalPages = 1 + (total-1)/p.Size
	details.HasNext = p.Page < details.TotalPages
	if p.Page > details.TotalPages {
		return total, 0, details
	}
	offset = (p.Page - 1) * p.Size
	return offset, min(p.Size, total-offset), details
}

type Store interface {
	LatestDMFromSenders(ctx context.Context, receiver int64) (map[int64]time.Time, error)
	// SetDMSubscription is called by trusted subscription handling. A new grant
	// creates one conversation and welcome message. Repeated grants reopen it.
	SetDMSubscription(ctx context.Context, members Metadata, active bool, welcome string) (int64, error)
	FindDMConversation(ctx context.Context, userID, otherID int64) (id int64, canMessage bool, err error)
	DMConversations(ctx context.Context, userID int64, page PageRequest) (Page[Conversation], error)
	DMMessages(ctx context.Context, userID, conversationID int64, page PageRequest) (Page[Message], error)
	UnreadDM(ctx context.Context, userID int64) (int64, error)
	ReadDM(ctx context.Context, userID, conversationID int64) error
	HideDM(ctx context.Context, userID, conversationID int64) error
	CheckDMSend(ctx context.Context, input SendInput) error
	SendDM(ctx context.Context, input SendInput) (Message, error)
}

type Moderator interface {
	ApproveDirectMessage(ctx context.Context, input SendInput) (bool, error)
}
