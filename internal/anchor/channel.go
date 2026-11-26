package anchor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"strings"
	"time"
)

var ErrChannelUnavailable = errors.New("channel reporting data is unavailable")

type ChannelFilter struct {
	Channel  string
	From, To *time.Time
}

type ChannelTotals struct {
	Users, NewUsers, Buyers, Transactions, Points, RevenueRatio int64
}

type ChannelStore interface {
	ChannelReport(context.Context, ChannelFilter) (ChannelTotals, error)
}

type ChannelUser struct {
	ID      int64
	Channel string
	Joined  *time.Time
}

type ChannelSpend struct {
	ID             string
	UserID, Points int64
	Created        time.Time
}

type ChannelRate struct {
	Channel string
	Ratio   int32
}

type channelPrincipal struct {
	Channel string    `json:"channel"`
	Hash    string    `json:"keyHash"`
	Expires time.Time `json:"expiresAt"`
}

type ChannelAuth struct {
	principals map[[sha256.Size]byte]channelPrincipal
	divisor    *big.Rat
}

func NewChannelAuth(file, conversion string) (*ChannelAuth, error) {
	if conversion == "" {
		conversion = "10"
	}
	ratio, ok := new(big.Rat).SetString(conversion)
	if !ok || ratio.Sign() <= 0 {
		return nil, errors.New("LP_TO_DOLLAR must be positive")
	}
	a := &ChannelAuth{principals: make(map[[sha256.Size]byte]channelPrincipal), divisor: ratio}
	if file == "" {
		return a, nil
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var records []channelPrincipal
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, err
	}
	for _, record := range records {
		key, err := hex.DecodeString(record.Hash)
		if err != nil || len(key) != sha256.Size || strings.TrimSpace(record.Channel) == "" || record.Expires.IsZero() {
			return nil, errors.New("invalid anchor channel credential")
		}
		var digest [sha256.Size]byte
		copy(digest[:], key)
		if _, dup := a.principals[digest]; dup {
			return nil, errors.New("duplicate anchor channel credential")
		}
		a.principals[digest] = record
	}
	return a, nil
}

func (a *ChannelAuth) Channel(header string) (string, bool) {
	token, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || len(token) < 32 || len(token) > 2048 {
		return "", false
	}
	record, found := a.principals[sha256.Sum256([]byte(token))]
	return record.Channel, found && time.Now().Before(record.Expires)
}

func (a *ChannelAuth) Amounts(points, ratio int64) (income, share string) {
	gross := new(big.Rat).Quo(new(big.Rat).SetInt64(points), a.divisor)
	net := new(big.Rat).Mul(gross, big.NewRat(ratio, 10000))
	return gross.FloatString(2), net.FloatString(2)
}
