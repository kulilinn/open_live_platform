package gift

import (
	"cmp"
	"context"
	"errors"
	"math"
	"slices"
)

var ErrCatalogConflict = errors.New("gift catalog changed or gift ID already exists")

type CatalogEntry struct {
	Item    Gift
	Deleted bool
}

type Overrides interface {
	GiftOverrides(context.Context) ([]CatalogEntry, error)
	GiftOverride(context.Context, int64) (CatalogEntry, bool, error)
}

type Catalog struct {
	baseline map[int64]Gift
	source   Overrides
}

func NewCatalog(items []Gift, source Overrides) *Catalog {
	byID := make(map[int64]Gift, len(items))
	for _, item := range items {
		byID[item.ID] = item
	}
	return &Catalog{baseline: byID, source: source}
}

func (c *Catalog) Baseline(id int64) bool {
	_, exists := c.baseline[id]
	return exists
}

func (c *Catalog) Lookup(ctx context.Context, id int64) (Gift, bool, error) {
	if c.source != nil {
		entry, exists, err := c.source.GiftOverride(ctx, id)
		if err != nil {
			return Gift{}, false, err
		}
		if exists {
			return entry.Item, !entry.Deleted, nil
		}
	}
	item, ok := c.baseline[id]
	return item, ok, nil
}

func (c *Catalog) Items(ctx context.Context) ([]Gift, error) {
	merged := make(map[int64]Gift, len(c.baseline))
	for id, item := range c.baseline {
		merged[id] = item
	}
	if c.source != nil {
		overrides, err := c.source.GiftOverrides(ctx)
		if err != nil {
			return nil, err
		}
		for _, entry := range overrides {
			if entry.Deleted {
				delete(merged, entry.Item.ID)
			} else {
				merged[entry.Item.ID] = entry.Item
			}
		}
	}
	items := make([]Gift, 0, len(merged))
	for _, item := range merged {
		items = append(items, item)
	}
	slices.SortFunc(items, func(a, b Gift) int {
		if i := cmp.Compare(giftOrder(a), giftOrder(b)); i != 0 {
			return i
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return items, nil
}

func giftOrder(item Gift) int32 {
	if item.GiftSort == nil {
		return math.MaxInt32
	}
	return *item.GiftSort
}
