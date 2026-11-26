package auth

import (
	"testing"
	"time"
)

func TestEmailCodeAttemptsAndExpiry(t *testing.T) {
	store := NewStore()
	ctx := t.Context()
	const email = "account@example.org"
	digest := [32]byte{5}
	reserve := func() {
		t.Helper()
		if offer, err := store.ReserveEmailCode(ctx, email, 1, digest, time.Now().Add(time.Minute)); err != nil || !offer.New {
			t.Fatalf("reserve offer = %+v, error %v", offer, err)
		}
		if err := store.FinishEmailCode(ctx, email, digest, true); err != nil {
			t.Fatal(err)
		}
	}
	reserve()
	for range EmailCodeMaxAttempts {
		if _, ok, err := store.RedeemEmailCode(ctx, email, [32]byte{6}, UseCodeForLogin); err != nil || ok {
			t.Fatalf("wrong code accepted = %t, error = %v", ok, err)
		}
	}
	if _, ok, err := store.RedeemEmailCode(ctx, email, digest, UseCodeForLogin); err != nil || ok {
		t.Fatalf("code accepted after maximum attempts = %t, error = %v", ok, err)
	}
	// Simulate elapsed time without waiting for the actual challenge TTL.
	code := store.emailCodes[email]
	code.expiresAt = time.Now().Add(-time.Second)
	code.attempts = 0
	store.emailCodes[email] = code
	if _, ok, err := store.RedeemEmailCode(ctx, email, digest, UseCodeForLogin); err != nil || ok {
		t.Fatalf("expired code accepted = %t, error = %v", ok, err)
	}
	reserve()
	if id, ok, err := store.RedeemEmailCode(ctx, email, digest, UseCodeForLogin); err != nil || !ok || id != 1 {
		t.Fatalf("valid code redemption = %d, %t, %v", id, ok, err)
	}
	if _, ok, err := store.RedeemEmailCode(ctx, email, digest, UseCodeForLogin); err != nil || ok {
		t.Fatalf("code reused = %t, error = %v", ok, err)
	}
}
