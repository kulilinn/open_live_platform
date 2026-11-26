package app_test

import (
	"encoding/json"
	"testing"

	"open_live_platform/internal/catalog"
	"open_live_platform/internal/store/postgres"
)

func TestPostgresCatalogSeedPreservesOperatorEdits(t *testing.T) {
	api := startEditorialAPI(t)
	ctx := t.Context()
	db, err := postgres.Open(ctx, api.platform.Config.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Shutdown()
	parse := func(raw string) catalog.Snapshot {
		t.Helper()
		var snapshot catalog.Snapshot
		if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	first := parse(`{
		"versions":[{"name":"ios","curr_version":"1"}],
		"common_settings":[{"id":"white_user","content":"old@example.test"}],
		"bad_words":[{"id":1,"name":"hydrate.txt"},{"id":2,"name":"upload.txt"},{"id":3,"name":"delete.txt"},{"id":4,"name":"edit.txt"}]
	}`)
	if err := db.SeedPublicCatalog(ctx, first, nil); err != nil {
		t.Fatal(err)
	}
	const word = "/api/operation/system/bad_words/"
	adminForm(t, api, word+"edit", "upload.txt", "text/plain", []byte("Go upload"), map[string]string{"id": "2"}, 200)
	api.call(t, word+"delete", map[string]string{"id": "3"}, 200, nil)
	adminForm(t, api, word+"edit", "", "", nil, map[string]string{"id": "4", "description": "Go edit"}, 200)
	api.call(t, "/api/operation/platform/whiteuser/set", map[string]string{"content": "operator@example.test"}, 200, nil)
	repeat := parse(`{
		"versions":[{"name":"ios","curr_version":"2"}],
		"common_settings":[{"id":"white_user","content":"new@example.test"}],
		"bad_words":[{"id":1,"name":"hydrate.txt","file":"AP8="},{"id":2,"name":"upload.txt","file":"b2xk"},
			{"id":3,"name":"delete.txt","file":"b2xk"},{"id":4,"name":"edit.txt","file":"b2xk"}]
	}`)
	for range 2 {
		if err := db.SeedPublicCatalog(ctx, repeat, nil); err != nil {
			t.Fatal(err)
		}
	}
	assertVersionAndSetting := func(wantVersion, wantSetting string) {
		t.Helper()
		versions, err := db.CatalogRows(ctx, "versions")
		if err != nil || len(versions) != 1 || versions[0].String("curr_version") != wantVersion {
			t.Fatalf("versions = %v: %v", versions, err)
		}
		got, err := db.AccessSetting(ctx, "white_user")
		if err != nil || got != wantSetting {
			t.Fatalf("white_user = %q: %v", got, err)
		}
	}
	assertVersionAndSetting("1", "operator@example.test")
	if err := db.SeedPublicCatalog(ctx, repeat, map[string]bool{"versions": true}); err != nil {
		t.Fatal(err)
	}
	assertVersionAndSetting("2", "operator@example.test")
	words, err := db.CatalogRows(ctx, "bad_words")
	if err != nil || len(words) != 3 || words[0].String("file") != "AP8=" || words[1].String("file") != "R28gdXBsb2Fk" ||
		words[2].Int("id") != 4 || words[2].String("description") != "Go edit" || words[2]["file"] != nil {
		t.Fatalf("word list after import = %v: %v", words, err)
	}
	if err := db.ReplacePublicCatalog(ctx, repeat); err != nil {
		t.Fatal(err)
	}
	assertVersionAndSetting("2", "new@example.test")
	words, err = db.CatalogRows(ctx, "bad_words")
	if err != nil || len(words) != 4 || words[1].String("file") != "b2xk" || words[2].Int("id") != 3 {
		t.Fatalf("word list after replacement = %v: %v", words, err)
	}
}
