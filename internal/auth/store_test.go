package auth

import (
	"errors"
	"testing"
)

func TestStoreSessions(t *testing.T) {
	store := NewStore()
	if _, err := store.SignUp(t.Context(), "creator@example.com", "", "Creator"); !errors.Is(err, ErrMissingFields) {
		t.Fatalf("incomplete signup error = %v", err)
	}
	signup, err := store.SignUp(t.Context(), " Creator@Example.com ", "a-test-password", "Creator")
	if err != nil {
		t.Fatal(err)
	}
	if signup.User.ID == 0 || signup.User.Email != "creator@example.com" || signup.User.Account != "Creator" {
		t.Fatalf("unexpected user: %+v", signup.User)
	}
	if user, ok, err := store.Bearer(t.Context(), "Bearer "+signup.Token); err != nil || !ok || user != signup.User {
		t.Fatalf("bearer lookup = %+v, %v", user, ok)
	}
	if _, ok, err := store.Bearer(t.Context(), signup.Token); err != nil || ok {
		t.Fatal("bare token accepted as an authorization header")
	}
	if _, ok, err := store.Lookup(t.Context(), "unknown"); err != nil || ok {
		t.Fatal("unknown token resolved to a user")
	}
	if _, err := store.SignUp(t.Context(), "creator@example.com", "a-test-password", "Duplicate"); !errors.Is(err, ErrEmailInUse) {
		t.Fatalf("duplicate signup error = %v", err)
	}
	if _, err := store.SignIn(t.Context(), "creator@example.com", "wrong-password"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("incorrect password error = %v", err)
	}
	signin, err := store.SignIn(t.Context(), "CREATOR@example.com", "a-test-password")
	if err != nil {
		t.Fatal(err)
	}
	if signin.User != signup.User || signin.Token == "" || signin.Token == signup.Token {
		t.Fatal("signin did not issue a new session for the same user")
	}
	if user, ok, err := store.Lookup(t.Context(), signin.Token); err != nil || !ok || user != signup.User {
		t.Fatalf("token lookup = %+v, %v", user, ok)
	}
	viewer, err := store.SignUp(t.Context(), "viewer@example.com", "another-test-password", "Viewer")
	if err != nil {
		t.Fatal(err)
	}
	if users, err := store.Users(t.Context()); err != nil || len(users) != 2 || users[0] != signup.User || users[1] != viewer.User {
		t.Fatalf("listed users = %+v", users)
	}
}
