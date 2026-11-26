package gift

import (
	"cmp"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"slices"
)

type Gift struct {
	ID                     int64   `json:"id,string"`
	Name                   string  `json:"name"`
	Price                  int64   `json:"price"`
	Icon                   string  `json:"icon"`
	GiftType               int8    `json:"giftType"`
	SpecialEffects         *string `json:"specialEffects"`
	Animation              *string `json:"animation"`
	NormalEffects          *string `json:"normalEffects"`
	IsCustom               *int8   `json:"isCustom"`
	GiftSort               *int32  `json:"giftSort"`
	RecommendedDescription *string `json:"recommendedDescription"`
	EffectControlType      int8    `json:"effectControlType"`
	VibrationIntensity     int8    `json:"vibrationIntensity"`
	ToyInspection          int8    `json:"toyInspection"`
	ParameterOne           *string `json:"parameterOne"`
	ParameterTwo           *int32  `json:"parameterTwo"`
	ParameterThree         *int64  `json:"parameterThree"`
	CreateBy               *string `json:"createBy"`
	CreateTime             *string `json:"createTime"`
	UpdateBy               *string `json:"updateBy"`
	UpdateTime             *string `json:"updateTime"`
	IncomeStream           int32   `json:"incomeStream"`
	PerformanceRule        int8    `json:"performanceRule"`
	PerformanceTime        int32   `json:"performanceTime"`
	Combo                  int8    `json:"combo"`
	HardwareQueueType      int8    `json:"hardwareQueueType"`
	Announcement           *string `json:"announcement"`
	AnnouncementOne        *string `json:"announcement1"`
	AnnouncementTwo        *string `json:"announcement2"`
	AnnouncementThree      *string `json:"announcement3"`
}

// Load reads the active catalog. An unset path represents an empty catalog.
func Load(path string) ([]Gift, error) {
	if path == "" {
		return []Gift{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var items []Gift
	if err := json.Unmarshal(data, &items); err != nil {
		return nil, err
	}
	if items == nil {
		return nil, fmt.Errorf("expected an array of gifts")
	}
	seen := make(map[int64]bool, len(items))
	for _, item := range items {
		if item.ID <= 0 || seen[item.ID] {
			return nil, fmt.Errorf("invalid or duplicate gift ID %d", item.ID)
		}
		seen[item.ID] = true
	}
	sortKey := func(item Gift) int32 {
		if item.GiftSort == nil {
			return math.MaxInt32
		}
		return *item.GiftSort
	}
	slices.SortStableFunc(items, func(a, b Gift) int {
		return cmp.Compare(sortKey(a), sortKey(b))
	})
	return items, nil
}
