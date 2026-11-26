package app_test

import (
	"slices"
	"testing"

	"open_live_platform/internal/app"
)

func TestPostgresJoinedRoomTranslationLanguages(t *testing.T) {
	cfg, db := postgresTestDatabase(t)
	ctx := t.Context()
	platform, err := app.New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(platform.Close)
	if _, err := db.Exec(ctx, `INSERT INTO users(id,email,account,password_salt,password_hash)
		VALUES (1,'host@example.org','host','',''),(2,'viewer@example.org','viewer','','')`); err != nil {
		t.Fatal(err)
	}
	roomLanguage := "fr"
	if _, err := platform.Rooms.StartWithLanguage(ctx, 1, "Live", "", &roomLanguage); err != nil {
		t.Fatal(err)
	}
	if err := platform.Accounts.SetTranslationLanguage(ctx, 2, "ja"); err != nil {
		t.Fatal(err)
	}
	if err := platform.Rooms.Join(ctx, 1, 1, ""); err != nil {
		t.Fatal(err)
	}
	peer, err := app.New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(peer.Close)
	joined, err := peer.Rooms.JoinRoom(ctx, 1, 2, "")
	if err != nil || !joined.AnchorJoined || joined.TranslationLanguage != "fr" || !slices.Equal(joined.Members, []int64{2}) {
		t.Fatalf("join on second instance = %+v, error = %v", joined, err)
	}
	languages, err := peer.Accounts.TranslationLanguages(ctx, joined.Members)
	if err != nil || languages[2] != "ja" {
		t.Fatalf("persisted viewer language = %v, error = %v", languages, err)
	}
	if err := peer.Rooms.Close(ctx, 1, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := platform.Rooms.Start(ctx, 1, "Next", ""); err != nil {
		t.Fatal(err)
	}
	joined, err = peer.Rooms.JoinRoom(ctx, 1, 2, "")
	if err != nil || joined.AnchorJoined || joined.TranslationLanguage != "fr" {
		t.Fatalf("join on next broadcast = %+v, error = %v", joined, err)
	}
}
