package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"open_live_platform/internal/catalog"
)

func attachWordListFiles(dir string, rows []catalog.Row) (int, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return 0, fmt.Errorf("open word-list directory: %w", err)
	}
	defer root.Close()
	count := 0
	for _, row := range rows {
		content, err := readWordListFile(root, row)
		if err != nil {
			return 0, fmt.Errorf("read word list %d: %w", row.Int("id"), err)
		}
		if content != nil {
			row["file"], err = json.Marshal(content)
			if err != nil {
				return 0, err
			}
			count++
		}
	}
	return count, nil
}

func localWordListPath(name string) (string, error) {
	if name == "." || !fs.ValidPath(name) || strings.ContainsAny(name, "\\:\x00") {
		return "", fs.ErrInvalid
	}
	return filepath.Localize(name)
}

func readWordListFile(root *os.Root, row catalog.Row) ([]byte, error) {
	var objectPath, objectName string
	if object, err := url.Parse(row.String("url")); err == nil {
		objectPath = strings.TrimPrefix(object.Path, "/")
		if _, err := localWordListPath(objectPath); err == nil {
			objectName = path.Base(objectPath)
		}
	}
	for _, candidate := range []string{objectPath, row.String("name"), objectName} {
		name, err := localWordListPath(candidate)
		if err != nil {
			continue
		}
		content, err := readRegularWordList(root, name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		return content, err
	}
	return nil, nil
}

func readRegularWordList(root *os.Root, name string) ([]byte, error) {
	info, err := root.Stat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%q is not a regular file", name)
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(file)
}
