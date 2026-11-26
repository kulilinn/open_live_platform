package dm

import (
	"cmp"
	"context"
	"slices"
	"strconv"
	"sync"
	"time"
)

type memoryConversation struct {
	Conversation
	hidden   map[int64]bool
	messages []Message
}

type Memory struct {
	mu            sync.Mutex
	conversations map[int64]*memoryConversation
	pairs         map[[2]int64]int64
	lastID        int64
}

func pair(first, second int64) [2]int64 {
	return [2]int64{min(first, second), max(first, second)}
}

func (m *Memory) SetDMSubscription(ctx context.Context, members Metadata, active bool, welcome string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	fanID, anchorID := members.Fan.ID, members.Anchor.ID
	if fanID < 1 || anchorID < 1 || fanID == anchorID || (active && !members.Anchor.Anchor) {
		return 0, ErrForbidden
	}
	key := pair(fanID, anchorID)
	if id, exists := m.pairs[key]; exists {
		m.conversations[id].CanMessage = active
		return id, nil
	}
	if !active {
		return 0, ErrNotFound
	}
	if m.pairs == nil {
		m.pairs = make(map[[2]int64]int64)
		m.conversations = make(map[int64]*memoryConversation)
	}
	m.lastID++
	id := m.lastID
	conversation := &memoryConversation{Conversation: Conversation{
		ID: id, Type: 1, Members: []string{strconv.FormatInt(anchorID, 10), strconv.FormatInt(fanID, 10)},
		Metadata: members, CreatedAt: time.Now().UTC(), CanMessage: true,
	}}
	m.conversations[id], m.pairs[key] = conversation, id
	m.appendMessage(conversation, SendInput{ConversationID: id, SenderID: anchorID, ReceiverID: fanID,
		Type: 1, Content: cmp.Or(welcome, Welcome)})
	return id, nil
}

func (m *Memory) FindDMConversation(ctx context.Context, userID, otherID int64) (int64, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	conversation := m.conversations[m.pairs[pair(userID, otherID)]]
	if conversation == nil {
		return 0, false, ErrNotFound
	}
	return conversation.ID, conversation.CanMessage, nil
}

func (m *Memory) DMConversations(ctx context.Context, userID int64, page PageRequest) (Page[Conversation], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Page[Conversation]{}, err
	}
	result := make([]Conversation, 0)
	for _, c := range m.conversations {
		other, member := otherParticipant(c.Metadata, userID)
		if !member || c.hidden[userID] {
			continue
		}
		conversation := c.Conversation
		conversation.OtherUser = other
		conversation.Members = slices.Clone(conversation.Members)
		for _, msg := range c.messages {
			if msg.ReceiverID == userID && msg.Status != Read {
				conversation.Unread++
			}
		}
		result = append(result, conversation)
	}
	slices.SortFunc(result, func(a, b Conversation) int {
		if order := b.LastActivity.Compare(a.LastActivity); order != 0 {
			return order
		}
		return cmp.Compare(b.ID, a.ID)
	})
	offset, limit, details := page.Window(int64(len(result)))
	return Page[Conversation]{Data: result[offset : offset+limit], Pagination: details}, nil
}

func (m *Memory) DMMessages(ctx context.Context, userID, conversationID int64, page PageRequest) (Page[Message], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Page[Message]{}, err
	}
	c, err := m.conversation(userID, conversationID)
	if err != nil {
		return Page[Message]{}, err
	}
	offset, limit, details := page.Window(int64(len(c.messages)))
	result := make([]Message, 0, limit)
	for i := range limit {
		result = append(result, c.messages[int64(len(c.messages))-1-offset-i])
	}
	return Page[Message]{Data: result, Pagination: details}, nil
}

func (m *Memory) UnreadDM(ctx context.Context, userID int64) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	var count int64
	for _, c := range m.conversations {
		for _, msg := range c.messages {
			if msg.ReceiverID == userID && msg.Status != Read {
				count++
			}
		}
	}
	return count, nil
}

func (m *Memory) ReadDM(ctx context.Context, userID, conversationID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	c, err := m.conversation(userID, conversationID)
	if err != nil {
		return err
	}
	for index := range c.messages {
		if c.messages[index].ReceiverID == userID {
			c.messages[index].Status = Read
		}
	}
	return nil
}

func (m *Memory) HideDM(ctx context.Context, userID, conversationID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	c, err := m.conversation(userID, conversationID)
	if err != nil {
		return err
	}
	if c.hidden == nil {
		c.hidden = make(map[int64]bool)
	}
	c.hidden[userID] = true
	return nil
}

func (m *Memory) CheckDMSend(ctx context.Context, input SendInput) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := m.sendableConversation(input)
	return err
}

func (m *Memory) SendDM(ctx context.Context, input SendInput) (Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Message{}, err
	}
	c, err := m.sendableConversation(input)
	if err != nil {
		return Message{}, err
	}
	return m.appendMessage(c, input), nil
}

func (m *Memory) conversation(userID, id int64) (*memoryConversation, error) {
	c := m.conversations[id]
	if c == nil {
		return nil, ErrNotFound
	}
	if _, member := otherParticipant(c.Metadata, userID); !member {
		return nil, ErrNotFound
	}
	return c, nil
}

func (m *Memory) sendableConversation(input SendInput) (*memoryConversation, error) {
	c, err := m.conversation(input.SenderID, input.ConversationID)
	if err != nil {
		return nil, err
	}
	other, _ := otherParticipant(c.Metadata, input.SenderID)
	if !c.CanMessage || other.ID != input.ReceiverID {
		return nil, ErrForbidden
	}
	return c, nil
}

func (m *Memory) appendMessage(c *memoryConversation, input SendInput) Message {
	m.lastID++
	msg := Message{
		ID: m.lastID, ConversationID: input.ConversationID, SenderID: input.SenderID, ReceiverID: input.ReceiverID,
		Type: input.Type, Content: input.Content, Status: Pending, CreatedAt: time.Now().UTC(),
	}
	c.messages = append(c.messages, msg)
	c.LastMessage, c.LastActivity, c.hidden = msg.Content, msg.CreatedAt, nil
	return msg
}

func otherParticipant(members Metadata, userID int64) (User, bool) {
	switch userID {
	case members.Fan.ID:
		return members.Anchor, true
	case members.Anchor.ID:
		return members.Fan, true
	default:
		return User{}, false
	}
}
