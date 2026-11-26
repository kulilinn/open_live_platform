package app_test

import (
	"encoding/json"
	"testing"
	"time"

	"open_live_platform/internal/store/postgres"
)

func TestSubscriptionExpirationReminders(t *testing.T) {
	cfg, db := postgresTestDatabase(t)
	ctx := t.Context()
	open := func() *postgres.Store {
		t.Helper()
		s, e := postgres.Open(ctx, cfg.DatabaseURL)
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(s.Shutdown)
		return s
	}
	a, b := open(), open()
	owner, e := a.SignUp(ctx, "host@example.test", "password", "Host")
	if e != nil {
		t.Fatal(e)
	}
	fan, e := b.SignUp(ctx, "fan@example.test", "password", "Fan")
	if e != nil {
		t.Fatal(e)
	}
	now := time.Date(2026, 11, 15, 13, 0, 0, 0, time.UTC)
	expires := now.Add(7 * 24 * time.Hour)
	var plan int64
	if e := db.QueryRow(ctx, "INSERT INTO subscription_plans(anchor_id,plan_type,price,name) VALUES($1,3,100,'Monthly') RETURNING id", owner.User.ID).Scan(&plan); e != nil {
		t.Fatal(e)
	}
	if _, e := db.Exec(ctx, "INSERT INTO subscription_members(fan_id,anchor_id,plan_id,created_at,expires_at) VALUES($1,$2,$3,$4,$5)", fan.User.ID, owner.User.ID, plan, now.AddDate(0, 0, -20), expires); e != nil {
		t.Fatal(e)
	}
	if n, e := a.SendSubscriptionReminders(ctx, now.Add(-time.Second)); e != nil || n != 0 {
		t.Fatal(n, e)
	}
	type result struct {
		n int64
		e error
	}
	done := make(chan result, 2)
	for _, s := range []*postgres.Store{a, b} {
		go func() { n, e := s.SendSubscriptionReminders(ctx, now); done <- result{n, e} }()
	}
	var sent int64
	for range 2 {
		r := <-done
		if r.e != nil {
			t.Fatal(r.e)
		}
		sent += r.n
	}
	if sent != 2 {
		t.Fatalf("delivered %d notices", sent)
	}
	for _, pair := range []struct {
		id           int64
		kind, target int
	}{{owner.User.ID, 3, 1}, {fan.User.ID, 6, 2}} {
		var kind, target int
		var raw []byte
		if e := db.QueryRow(ctx, "SELECT notice_type,notice_target,content FROM notices WHERE receive_id=$1", pair.id).Scan(&kind, &target, &raw); e != nil {
			t.Fatal(e)
		}
		var info struct {
			Plan string `json:"plan_name"`
		}
		if e := json.Unmarshal(raw, &info); e != nil || kind != pair.kind || target != pair.target || info.Plan != "Monthly" {
			t.Fatalf("notice %d/%d %s", kind, target, raw)
		}
	}
	if n, e := a.SendSubscriptionReminders(ctx, expires); e != nil || n != 0 {
		t.Fatal(n, e)
	}
}
