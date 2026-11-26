package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"open_live_platform/internal/catalog"
)

func wordListRow(url, name string) catalog.Row {
	object, _ := json.Marshal(url)
	fileName, _ := json.Marshal(name)
	return catalog.Row{"url": object, "name": fileName}
}

func TestAttachLegacyWordListFiles(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "snapshot")
	if err := os.MkdirAll(filepath.Join(dir, "library"), 0700); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"snapshot/library/禁止 語.txt": {0xef, 0xbb, 0xbf, 0, 0xff},
		"snapshot/original.txt":     []byte("by-file-name"),
		"snapshot/flat.txt":         []byte("by-url-basename"),
		"snapshot/empty.txt":        {},
		"outside.txt":               []byte("outside of snapshot"),
	}
	for path, data := range files {
		if err := os.WriteFile(filepath.Join(base, filepath.FromSlash(path)), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	rows := []catalog.Row{
		wordListRow("https://example.invalid/library/%E7%A6%81%E6%AD%A2%20%E8%AA%9E.txt?token=ignored", "original.txt"),
		wordListRow("https://example.invalid/missing/file.txt", "original.txt"),
		wordListRow("https://example.invalid/missing/flat.txt", ""),
		wordListRow("", "empty.txt"),
		wordListRow("https://example.invalid/%2e%2e/outside.txt", "../outside.txt"),
		wordListRow("", `..\outside.txt`),
		wordListRow("", filepath.Join(base, "outside.txt")),
		wordListRow("https://example.invalid/missing.txt", "missing.txt"),
	}
	n, err := attachWordListFiles(dir, rows)
	if err != nil || n != 4 {
		t.Fatalf("attached %d legacy files: %v", n, err)
	}
	wanted := [][]byte{files["snapshot/library/禁止 語.txt"], files["snapshot/original.txt"], files["snapshot/flat.txt"], {}}
	for i, expected := range wanted {
		var got []byte
		if err := json.Unmarshal(rows[i]["file"], &got); err != nil || got == nil || !bytes.Equal(got, expected) {
			t.Fatalf("file %d = %q, want %q: %v", i, got, expected, err)
		}
	}
	for _, row := range rows[len(wanted):] {
		if file, ok := row["file"]; ok {
			t.Fatalf("unexpected file %s", file)
		}
	}
}

func TestLegacyWordListSymlinkCannotLeaveSnapshot(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	dir := filepath.Join(base, "snapshot")
	if err := os.Mkdir(outside, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "words.txt"), []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "library")
	if err := os.Symlink(outside, link); err != nil {
		if runtime.GOOS != "windows" {
			t.Fatal(err)
		}
		if output, err := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", link, outside).CombinedOutput(); err != nil {
			t.Fatalf("create test junction: %v: %s", err, output)
		}
	}
	if _, err := os.Stat(filepath.Join(link, "words.txt")); err != nil {
		t.Fatalf("test link must reach the outside file: %v", err)
	}
	rows := []catalog.Row{wordListRow("", "library/words.txt")}
	if _, err := attachWordListFiles(dir, rows); err == nil || rows[0]["file"] != nil {
		t.Fatalf("outside symlink was accepted: %v", err)
	}
}

func TestCatalogImportOptions(t *testing.T) {
	var output strings.Builder
	opts, err := parseOptions(nil, &output)
	if err != nil || opts.replace != "none" {
		t.Fatalf("default options = %+v: %v", opts, err)
	}
	if _, err := parseOptions([]string{"-replace=everything"}, &output); err == nil {
		t.Fatal("unknown replacement mode accepted")
	}
}
