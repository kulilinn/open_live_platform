package gift

import (
	"context"
	"errors"
)

var ErrBlocked = errors.New("gift is blocked for this session")

type BlockStore interface {
	BlockDateGifts(ctx context.Context, channel, actor int64, ids []int64) error
	BlockedDateGifts(ctx context.Context, channel int64) (map[int64]struct{}, error)
}
