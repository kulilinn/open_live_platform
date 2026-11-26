package botchat

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const PageSize = 20

var ErrCursor = errors.New("invalid bot chat cursor")
var xidEncoding = base32.NewEncoding("0123456789abcdefghijklmnopqrstuv").WithPadding(base32.NoPadding)

type Message struct {
	ID        string   `json:"id"`
	From      int64    `json:"from,string"`
	To        int64    `json:"to,string"`
	Message   string   `json:"message"`
	Translate string   `json:"translate"`
	Language  string   `json:"language"`
	TTS       string   `json:"tts"`
	Image     []string `json:"image"`
	Video     []string `json:"video"`
	Time      string   `json:"time"`
}

type Store interface {
	SaveBotChat(context.Context, Message) error
	BotChatHistory(ctx context.Context, user, bot int64, after string) ([]Message, error)
}

func Channel(a, b int64) string {
	return fmt.Sprintf("%x", md5.Sum([]byte(strconv.FormatInt(min(a, b), 10)+strconv.FormatInt(max(a, b), 10))))
}

func NewMessage(from, to int64, body, language string) Message {
	now := time.Now().Unix()
	var id [12]byte
	binary.BigEndian.PutUint32(id[:4], uint32(now))
	rand.Read(id[4:])
	return Message{ID: fmt.Sprintf("%s/%d/%s", Channel(from, to), 9999999999-now, xidEncoding.EncodeToString(id[:])), From: from, To: to, Message: body, Language: language, Time: strconv.FormatInt(now, 10), Image: []string{}, Video: []string{}}
}

func After(user, bot int64, cursor string) (string, error) {
	if cursor == "latest" {
		return "", nil
	}
	data, err := base64.RawURLEncoding.DecodeString(cursor)
	id, ok := strings.CutPrefix(string(data), pairPrefix(user, bot))
	if err != nil || !ok || id == "" {
		return "", ErrCursor
	}
	return id, nil
}

func pairPrefix(a, b int64) string { return fmt.Sprintf("%d/%d/", min(a, b), max(a, b)) }

type Page struct {
	Status string    `json:"status"`
	Data   []Message `json:"data"`
	More   string    `json:"more"`
}

func Paginate(messages []Message) Page {
	p := Page{Status: "200", Data: messages}
	if len(messages) > PageSize {
		p.Data = messages[:PageSize]
		last := p.Data[PageSize-1]
		p.More = base64.RawURLEncoding.EncodeToString([]byte(pairPrefix(last.From, last.To) + last.ID))
	}
	return p
}

type Memory struct {
	mu       sync.RWMutex
	messages map[string]Message
}

func (s *Memory) SaveBotChat(_ context.Context, m Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.messages == nil {
		s.messages = make(map[string]Message)
	}
	s.messages[m.ID] = m
	return nil
}
func (s *Memory) BotChatHistory(_ context.Context, user, bot int64, after string) ([]Message, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := []Message{}
	for _, m := range s.messages {
		if m.ID > after && ((m.From == user && m.To == bot) || (m.From == bot && m.To == user)) {
			items = append(items, m)
		}
	}
	slices.SortFunc(items, func(a, b Message) int { return strings.Compare(a.ID, b.ID) })
	return items[:min(len(items), PageSize+1)], nil
}
