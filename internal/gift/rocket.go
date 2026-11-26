package gift

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"sort"
	"strconv"
)

const RefuelingEffect int8 = 7

var (
	ErrRocketUnavailable = errors.New("rocket reward configuration unavailable")
	ErrCannotRefuel      = errors.New("rocket cannot be refueled")
)

type RocketRules struct {
	InitialJackpot   int64            `json:"initial_jackpot"`
	PlatformRatio    *float32         `json:"platform_income_ratio"`
	Rewards          []RocketReward   `json:"rewards"`
	TimeRules        []RocketTimeRule `json:"time_rules"`
	Notices          []RocketNotice   `json:"notices"`
	MinSeconds       int              `json:"-"`
	SpeedAttenuation int32            `json:"speed_attenuation"`
}

type RocketReward struct {
	ID            int64 `json:"id,string"`
	GiftID        int64 `json:"gift_id,string"`
	PoolCondition int64 `json:"pool_condition"`
	Weight        int64 `json:"weight"`
	Value         int64 `json:"reward_value"`
	Multiplier    int64 `json:"reward_multiplier"`
	PlatformExp   int64 `json:"platform_exp"`
	FuelPoints    int64 `json:"fuel_points"`
	Gift          Gift  `json:"gift,omitempty"`
}

type RocketTimeRule struct {
	Condition   int64 `json:"time_condition"`
	Coefficient int64 `json:"time_coefficient"`
}

type RocketNotice struct {
	Type        int8   `json:"notice_type"`
	Condition   int64  `json:"config_condition"`
	RoomFlag    int8   `json:"live_room_flag"`
	MarqueeFlag int8   `json:"horse_race_lamp_flag"`
	Content     string `json:"content"`
	GiftID      int64  `json:"gift_id,string"`
	BroadcastID int64  `json:"broadcast_config_id,string"`
	Deleted     int8   `json:"del_flag"`
	Gift        *Gift  `json:"gift,omitempty"`
}

const (
	CriticalRocketNotice int8 = 1
	ComboRocketNotice    int8 = 2
)

type RefuelPlan struct {
	Contribution, PlatformIncome, Drawn, Reserved int64
	FuelMillis, Score, Multiplier, PlatformExp    int64
	Rewards                                       []RocketReward
	Notices                                       []RocketNotice
}

func LoadRocketRules(path string, catalog []Gift) (*RocketRules, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	rules := &RocketRules{MinSeconds: 3}
	if err = json.Unmarshal(raw, rules); err != nil {
		return nil, err
	}
	if value, ok := os.LookupEnv("ROCKET_REFUEL_DURATION_CONFIG"); ok {
		rules.MinSeconds, err = strconv.Atoi(value)
		if err != nil {
			return nil, fmt.Errorf("invalid ROCKET_REFUEL_DURATION_CONFIG: %w", err)
		}
	}
	if rules.InitialJackpot < 0 || rules.PlatformRatio == nil || *rules.PlatformRatio < 0 || *rules.PlatformRatio > 1 ||
		rules.MinSeconds < 0 || rules.MinSeconds > 3600 || rules.SpeedAttenuation < 0 || len(rules.Rewards) == 0 || len(rules.TimeRules) == 0 {
		return nil, ErrRocketUnavailable
	}
	items := make(map[int64]Gift, len(catalog))
	for _, item := range catalog {
		items[item.ID] = item
	}
	seen := make(map[int64]bool, len(rules.Rewards))
	var weight int64
	for i := range rules.Rewards {
		reward := &rules.Rewards[i]
		item, ok := items[reward.GiftID]
		if !ok || seen[reward.ID] || reward.ID <= 0 || reward.PoolCondition < 0 || reward.Weight <= 0 ||
			reward.Weight > math.MaxInt32-weight || reward.Value < 0 || reward.Value > math.MaxInt32 ||
			reward.Multiplier < 0 || reward.Multiplier > math.MaxInt32 || reward.PlatformExp < 0 || reward.PlatformExp > math.MaxInt32 ||
			reward.FuelPoints < 0 || reward.FuelPoints > math.MaxInt32 || item.Price != 0 || item.EffectControlType != 0 || item.IncomeStream < 0 {
			return nil, fmt.Errorf("invalid rocket reward %d", reward.ID)
		}
		seen[reward.ID] = true
		reward.Gift = item
		weight += reward.Weight
	}
	for _, term := range rules.TimeRules {
		if term.Condition <= 0 || term.Condition > math.MaxInt32 || term.Coefficient < 0 || term.Coefficient > math.MaxInt32 {
			return nil, fmt.Errorf("invalid rocket time rule")
		}
	}
	for i := range rules.Notices {
		notice := &rules.Notices[i]
		if notice.Deleted != 0 {
			continue
		}
		if (notice.Type != CriticalRocketNotice && notice.Type != ComboRocketNotice) || notice.Condition < 1 || notice.Condition > math.MaxInt32 {
			return nil, fmt.Errorf("invalid rocket notice condition %d", notice.Condition)
		}
		if item, found := items[notice.GiftID]; found {
			notice.Gift = &item
		}
	}
	sort.SliceStable(rules.Rewards, func(i, j int) bool { return rules.Rewards[i].PoolCondition < rules.Rewards[j].PoolCondition })
	sort.SliceStable(rules.TimeRules, func(i, j int) bool { return rules.TimeRules[i].Condition < rules.TimeRules[j].Condition })
	sort.SliceStable(rules.Notices, func(i, j int) bool { return rules.Notices[i].Condition < rules.Notices[j].Condition })
	return rules, nil
}

func (r *RocketRules) LaunchAmounts(cost int64) (int64, int64) {
	return int64((1 - *r.PlatformRatio) * float32(cost)), int64(*r.PlatformRatio * float32(cost))
}

func (r *RocketRules) Draw(order Order, available, flightSeconds int64) (RefuelPlan, error) {
	count := int64(order.Input.Quantity)
	if count <= 0 || count > 100 || order.Cost <= 0 || order.Cost > math.MaxInt32 {
		return RefuelPlan{}, ErrUnsupported
	}
	plan := RefuelPlan{
		Contribution:   int64((1-*r.PlatformRatio)*float32(order.Cost/count)) * count,
		PlatformIncome: int64(*r.PlatformRatio * float32(order.Cost)),
		Notices:        r.Notices,
	}
	if available > math.MaxInt64-plan.Contribution {
		return RefuelPlan{}, ErrRocketUnavailable
	}
	balance := available + plan.Contribution
	for range count {
		var total int64
		for _, reward := range r.Rewards {
			if reward.PoolCondition > balance {
				break
			}
			total += reward.Weight
		}
		if total == 0 {
			break
		}
		draw := rand.Int64N(total)
		var chosen RocketReward
		for _, reward := range r.Rewards {
			draw -= reward.Weight
			if draw < 0 {
				chosen = reward
				break
			}
		}
		if chosen.Value > balance {
			break
		}
		balance -= chosen.Value
		plan.Drawn += chosen.Value
		plan.Score += chosen.Value
		plan.Multiplier += chosen.Multiplier
		plan.PlatformExp += chosen.PlatformExp
		plan.FuelMillis += chosen.FuelPoints
		plan.Rewards = append(plan.Rewards, chosen)
	}
	if int64(len(plan.Rewards)) != count {
		// Guarantee the configured draws before requesting payment.
		return RefuelPlan{}, ErrRocketUnavailable
	}
	plan.Reserved = max(0, plan.Drawn-plan.Contribution)
	coefficient := int64(0)
	for _, term := range r.TimeRules {
		if term.Condition > flightSeconds {
			coefficient = term.Coefficient
			break
		}
	}
	if coefficient != 0 && plan.FuelMillis > math.MaxInt64/coefficient {
		return RefuelPlan{}, ErrRocketUnavailable
	}
	plan.FuelMillis *= coefficient
	if plan.FuelMillis > math.MaxInt64/1_000_000 {
		return RefuelPlan{}, ErrRocketUnavailable
	}
	return plan, nil
}
