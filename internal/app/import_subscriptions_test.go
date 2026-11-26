package app_test

import (
	"errors"
	"testing"
	"time"

	"open_live_platform/internal/dm"
	"open_live_platform/internal/store/postgres"
	"open_live_platform/internal/subscription"
)

func TestPostgresImportLiveMediaSubscriptions(t *testing.T) {
	config, db := postgresTestDatabase(t)
	ctx := t.Context()
	store, err := postgres.Open(ctx, config.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Shutdown()
	users := []int64{}
	for _, email := range []string{"legacy-fan@example.org", "legacy-anchor@example.org", "legacy-viewer@example.org"} {
		user, err := store.SignUp(ctx, email, "password", email)
		if err != nil {
			t.Fatal(err)
		}
		users = append(users, user.User.ID)
	}
	fan, anchor, viewer := users[0], users[1], users[2]
	now := time.Now().UTC().Truncate(time.Microsecond)
	const oldFree, latestFree, paid = int64(79591199061446659), int64(79591199061446660), int64(79591199061446661)
	firstPlan := postgres.LegacySubscriptionPlan{Plan: subscription.Plan{ID: oldFree, AnchorID: anchor, Published: 1,
		Type: subscription.Free, Name: "Older Free plan", CreatedAt: now.Add(-24 * time.Hour), UpdatedAt: now}}
	latestPlan := postgres.LegacySubscriptionPlan{Plan: subscription.Plan{ID: latestFree, AnchorID: anchor, Published: 1,
		Type: subscription.Free, Name: "Newest Free plan", CreatedAt: now, UpdatedAt: now}}
	paidPlan := postgres.LegacySubscriptionPlan{Plan: subscription.Plan{ID: paid, AnchorID: anchor, Published: 1, Promotion: 1,
		Type: subscription.Basic, Price: 990, Name: "Legacy Paid", Bundle: `[{"id":"9"}]`, Promote: `{"promotion_time":7}`, CreatedAt: now, UpdatedAt: now}}
	membership := postgres.LegacySubscriptionMember{Relationship: subscription.Relationship{ID: "old-relationship-id",
		FanID: fan, AnchorID: anchor, PlanID: paid, Level: 1, Bundle: "9", CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour)},
		PromotionExpiresAt: now.Add(-time.Hour)}
	linked := postgres.LegacySubscriptionSettlement{ID: 100, No: "legacy-settlement", FanID: fan, AnchorID: anchor,
		Type: 6, Status: 2, SettleLD: 99, Points: 99, TransactionID: 1000, TransactionNo: "point-transaction", CreatedAt: now,
		SettleTime: &now, SettleRatio: "0.8125", ActualSettleLD: 80, VerifiedPlanID: &membership.PlanID}
	unlinked := linked
	unlinked.ID, unlinked.No, unlinked.TransactionID, unlinked.TransactionNo, unlinked.VerifiedPlanID = 101, "remote-order", 1001, "unknown-reference", nil
	importRows := func() postgres.SubscriptionImportCounts {
		t.Helper()
		counts, err := store.ImportLegacySubscriptions(ctx, legacyRows(firstPlan, latestPlan, paidPlan), legacyRows(membership), legacyRows(linked, unlinked))
		if err != nil {
			t.Fatal(err)
		}
		return counts
	}
	if result := importRows(); result != (postgres.SubscriptionImportCounts{Plans: 3, Memberships: 1, Settlements: 2, LinkedSettlements: 1}) {
		t.Fatalf("first subscription import = %+v", result)
	}
	var verified, archived int
	if err := db.QueryRow(ctx, `SELECT count(verified_plan_id),count(*) FROM legacy_subscription_settlements WHERE fan_id=$1`, fan).Scan(&verified, &archived); err != nil || verified != 1 || archived != 2 {
		t.Fatalf("archived orders = %d, linked = %d, error = %v", archived, verified, err)
	}
	rows, err := store.Relationships(ctx, fan, anchor)
	if err != nil || len(rows) != 1 || rows[0].ID != membership.ID || rows[0].Bundle != membership.Bundle || !rows[0].ExpiresAt.Equal(membership.ExpiresAt) {
		t.Fatalf("legacy entitlement = %+v, error = %v", rows, err)
	}
	plans, err := store.Plans(ctx, anchor, false)
	if err != nil || len(plans) != 3 {
		t.Fatalf("legacy plans = %+v, error = %v", plans, err)
	}
	for _, p := range plans {
		if p.ID == paid && (p.Promotion != 0 || p.Promote != "" || p.Bundle != paidPlan.Bundle) {
			t.Fatalf("historical promotion became claimable or bundle was lost: %+v", p)
		}
	}
	// Even a matching live-ledger receipt cannot make an imported entitlement renewable by itself.
	if _, err := db.Exec(ctx, `INSERT INTO subscription_orders(order_no,fan_id,plan_id,points,status,provider_order_id,provider_order_no,confirmed_at)
		VALUES('existing-verified-order',$1,$2,99,1,1000,'point-transaction',now())`, fan, paid); err != nil {
		t.Fatal(err)
	}
	if candidates, err := store.RenewalCandidates(ctx, now.Add(48*time.Hour)); err != nil || len(candidates) != 0 {
		t.Fatalf("legacy entitlement eligible for surprise debit: %+v, %v", candidates, err)
	}
	if _, err := store.ReserveRenewal(ctx, subscription.RenewalCandidate{FanID: fan, AnchorID: anchor, PlanID: paid, ExpiresAt: membership.ExpiresAt}, "test-renew"); !errors.Is(err, subscription.ErrRenewalIneligible) {
		t.Fatalf("legacy entitlement accepted as renewal: %v", err)
	}
	if err := store.SubscribeFree(ctx, dm.Metadata{Fan: dm.User{ID: viewer}, Anchor: dm.User{ID: anchor, Anchor: true}}); err != nil {
		t.Fatal(err)
	}
	if rows, err := store.Relationships(ctx, viewer, anchor); err != nil || len(rows) != 1 || rows[0].PlanID != latestFree {
		t.Fatalf("free subscription used wrong legacy plan: %+v, %v", rows, err)
	}
	if err := store.ChangePlan(ctx, anchor, paid, subscription.PlanChange{Fields: &subscription.PlanInput{Name: "Go edited", Price: 990}}); err != nil {
		t.Fatal(err)
	}
	if replay := importRows(); replay != (postgres.SubscriptionImportCounts{Existing: 6}) {
		t.Fatalf("reimport overwrote a changing target: %+v", replay)
	}
	if rows, err := store.Relationships(ctx, fan, anchor); err != nil || len(rows) != 1 || rows[0].PlanName != "Go edited" {
		t.Fatalf("reimport overwrote local plan edit: %+v, %v", rows, err)
	}
	if next, err := store.CreatePlan(ctx, anchor, subscription.Basic, subscription.PlanInput{Name: "New", Price: 990}); err != nil || next.ID <= paid {
		t.Fatalf("plan identity generator did not advance: %d, %v", next.ID, err)
	}
}
