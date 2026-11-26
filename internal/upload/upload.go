package upload

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"iter"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

type Config struct {
	Bucket, Endpoint, Region, KeyID, Secret, Parent, BaseURL string
	ChunkSize, MaxImage, MaxVideo                            int64
}

func (c Config) Ready() bool { return c.Bucket != "" && c.KeyID != "" && c.Secret != "" }
func (c Config) Limit(name string) int64 {
	switch strings.ToLower(path.Ext(name)) {
	case ".mp4", ".avi", ".mkv", ".mov", ".flv":
		return c.MaxVideo
	}
	return c.MaxImage
}

func ValidName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\\x00") && len(name) <= 255
}

type Part struct {
	UserID       int64
	Name         string
	Index, Count int
	Data         []byte
	Limit        int64
}

func (p Part) Key() string    { return "assets/files/" + strconv.FormatInt(p.UserID, 10) + "/" + p.Name }
func Token(key string) string { sum := sha256.Sum256([]byte(key)); return hex.EncodeToString(sum[:]) }

var ErrConflict = errors.New("upload part differs from prior request")
var ErrLimit = errors.New("uploaded file too large")

type Parts = iter.Seq2[[]byte, error]
type Finish func(context.Context, string, Parts) error
type Store interface {
	StageUpload(context.Context, Part, Finish) error
}

// Put assembles the browser's small, potentially out-of-order chunks on disk.
// S3 receives one signed PUT, so 256 KB browser chunks require no client changes.
func (c Config) Put(ctx context.Context, key string, parts Parts) error {
	file, err := os.CreateTemp("", "livemedia-upload-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	hash := sha256.New()
	writer := io.MultiWriter(file, hash)
	var size int64
	for part, err := range parts {
		if err != nil {
			return err
		}
		n, err := writer.Write(part)
		if err != nil {
			return err
		}
		size += int64(n)
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	region := cmp.Or(c.Region, "ap-northeast-1")
	endpoint := cmp.Or(c.Endpoint, "https://s3."+region+".amazonaws.com")
	addr, err := url.Parse(endpoint)
	if err != nil {
		return err
	}
	addr.Path = path.Join(addr.Path, c.Bucket, c.Parent, key)
	addr.RawPath = ""
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, addr.String(), file)
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", cmp.Or(mime.TypeByExtension(path.Ext(key)), "application/octet-stream"))
	digest := hex.EncodeToString(hash.Sum(nil))
	req.Header.Set("X-Amz-Content-Sha256", digest)
	creds := aws.Credentials{AccessKeyID: c.KeyID, SecretAccessKey: c.Secret}
	if err := v4.NewSigner().SignHTTP(ctx, creds, req, digest, "s3", region, time.Now().UTC(), func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true }); err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: 2 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("S3 PUT returned %d", resp.StatusCode)
	}
	return nil
}

type batch struct {
	mu    sync.Mutex
	count int
	parts map[int][]byte
	size  int64
	done  bool
	timer *time.Timer
}

type Memory struct {
	mu    sync.Mutex
	files map[string]*batch
}

func (m *Memory) StageUpload(ctx context.Context, p Part, finish Finish) error {
	key := p.Key()
	m.mu.Lock()
	if m.files == nil {
		m.files = make(map[string]*batch)
	}
	b := m.files[key]
	if b == nil {
		b = &batch{count: p.Count, parts: make(map[int][]byte)}
		m.files[key] = b
	}
	if b.timer != nil {
		b.timer.Stop()
	}
	// The map and timer identity prevent an old timer from evicting a retry.
	var expiry *time.Timer
	expiry = time.AfterFunc(30*time.Minute, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.files[key] == b && b.timer == expiry {
			delete(m.files, key)
		}
	})
	b.timer = expiry
	m.mu.Unlock()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.count != p.Count {
		return ErrConflict
	}
	if b.done {
		return nil
	}
	if old, ok := b.parts[p.Index]; ok {
		if !bytes.Equal(old, p.Data) {
			return ErrConflict
		}
	} else {
		if b.size+int64(len(p.Data)) > p.Limit {
			return ErrLimit
		}
		b.size += int64(len(p.Data))
		b.parts[p.Index] = p.Data
	}
	if len(b.parts) != b.count {
		return nil
	}
	err := finish(ctx, key, func(yield func([]byte, error) bool) {
		for index := 0; index < b.count; index++ {
			if !yield(b.parts[index], nil) {
				return
			}
		}
	})
	if err == nil {
		b.done = true
		b.parts = nil
	}
	return err
}
