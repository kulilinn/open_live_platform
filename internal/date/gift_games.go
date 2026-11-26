package date

import (
	"context"
	"errors"
	"time"

	"open_live_platform/internal/gift"
)

var (
	ErrGameBusy      = errors.New("another gift game is active")
	ErrGameNotReady  = errors.New("no active gift game")
	ErrGameMove      = errors.New("invalid game move")
	ErrGameQuestions = errors.New("gift game question catalog is unavailable")
)

const (
	GameVoting = iota
	GamePayment
	GamePlaying
	GameFinished
	GameRejected
)

const GiftGameLifetime = 9000 * time.Second

type GiftGame struct {
	ID, Channel, OrderID, GiftID int64
	Kind                         uint8
	Anchor, Audience             int64
	Stage                        int
	Expires                      time.Time
	Votes                        [2]*bool
	Questions                    []gift.Question
	Quan                         [2]*uint8
	Guesses                      [2]map[int64]string
	Result                       *GameResult
	RejectedBy                   uint8
}

type GameMove struct {
	Kind     uint8
	Gesture  uint8
	Question int64
	Answer   string
}

type GameResult struct {
	Winner      uint8  `json:"winner"`
	WinnerScore uint16 `json:"winnerScore"`
	LoserScore  uint16 `json:"loserScore"`
	Kind        uint8  `json:"gameType"`
}

type GiftGameStore interface {
	PrepareDateGiftGame(ctx context.Context, channel, actor int64) error
	VoteDateGiftGame(ctx context.Context, candidate GiftOrder, actor int64, kind uint8, consent bool, questions []gift.Question) (GiftGame, bool, error)
	BeginPaidGiftGame(ctx context.Context, channel, gameID int64) (GiftGame, bool, error)
	MoveGiftGame(ctx context.Context, channel, actor int64, move GameMove) (*GameResult, error)
}

func (g GiftGame) Role(actor int64) uint8 {
	switch actor {
	case g.Anchor:
		return 1
	case g.Audience:
		return 2
	default:
		return 0
	}
}

func (g *GiftGame) Vote(actor int64, consent bool) bool {
	role := g.Role(actor)
	g.Votes[role-1] = &consent
	if g.Votes[0] == nil || g.Votes[1] == nil {
		return false
	}
	for i, vote := range g.Votes {
		if !*vote {
			g.RejectedBy, g.Stage = uint8(i+1), GameRejected
			return true
		}
	}
	g.Stage = GamePayment
	return true
}

func (g *GiftGame) Move(actor int64, move GameMove) (*GameResult, error) {
	role := g.Role(actor)
	if role == 0 {
		return nil, ErrForbidden
	}
	if g.Kind != move.Kind || g.Stage != GamePlaying {
		return nil, ErrGameNotReady
	}
	var result *GameResult
	if move.Kind == 0 {
		if move.Gesture > 3 {
			return nil, ErrGameMove
		}
		g.Quan[role-1] = &move.Gesture
		if g.Quan[0] != nil && g.Quan[1] != nil {
			result = quanResult(*g.Quan[0], *g.Quan[1])
		}
	} else {
		index := -1
		for j, q := range g.Questions {
			if q.ID == move.Question {
				index = j
				break
			}
		}
		if index < 0 || !g.Questions[index].Allows(move.Answer) {
			return nil, ErrGameMove
		}
		if g.Guesses[role-1] == nil {
			g.Guesses[role-1] = make(map[int64]string)
		}
		g.Guesses[role-1][move.Question] = move.Answer
		if len(g.Guesses[0]) == len(g.Questions) && len(g.Guesses[1]) == len(g.Questions) {
			result = g.guessResult()
		}
	}
	if result != nil {
		g.Result, g.Stage = result, GameFinished
	}
	return result, nil
}

func quanResult(anchor, audience uint8) *GameResult {
	switch {
	case anchor == 0:
		return &GameResult{Winner: 2, WinnerScore: 1}
	case audience == 0:
		return &GameResult{Winner: 1, WinnerScore: 1}
	case anchor == audience:
		return &GameResult{}
	case (anchor == 1 && audience == 2) || (anchor == 2 && audience == 3) || (anchor == 3 && audience == 1):
		return &GameResult{Winner: 1, WinnerScore: 1}
	default:
		return &GameResult{Winner: 2, WinnerScore: 1}
	}
}

func (g GiftGame) guessResult() *GameResult {
	var first, second uint16
	for _, q := range g.Questions {
		match := g.Guesses[0][q.ID] == g.Guesses[1][q.ID]
		// The opponent guesses the target's answer. Score one point per
		// question using the selected question's target and the two submitted choices.
		if (q.Target == 1 && !match) || (q.Target == 2 && match) {
			first++
		} else {
			second++
		}
	}
	if first > second {
		return &GameResult{Winner: 1, WinnerScore: first, LoserScore: second, Kind: 1}
	}
	if second > first {
		return &GameResult{Winner: 2, WinnerScore: second, LoserScore: first, Kind: 1}
	}
	return &GameResult{WinnerScore: first, LoserScore: second, Kind: 1}
}
