package upload

import (
	"bufio"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

var ErrPassNotFound = errors.New("wallet pass not found")

type PassConfig struct {
	IndexFile, AppName string
	Storage            Config
}

type Passes struct {
	index   map[string]passRecord
	storage Config
	client  *http.Client
}

type passRecord struct {
	Key       string    `json:"key"`
	Object    string    `json:"object"`
	ExpiresAt time.Time `json:"expires_at"`
}

func NewPasses(config PassConfig) (*Passes, error) {
	if config.IndexFile == "" {
		return nil, nil
	}
	if !config.Storage.Ready() {
		return nil, errors.New("wallet pass index requires AWS_PASSES_S3_BUCKET and S3 credentials")
	}
	index, err := os.Open(config.IndexFile)
	if err != nil {
		return nil, err
	}
	defer index.Close()
	passes := &Passes{index: make(map[string]passRecord), storage: config.Storage, client: &http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	suffix := "|pkpass|" + cmp.Or(config.AppName, "unknown")
	scanner := bufio.NewScanner(index)
	for line := 1; scanner.Scan(); line++ {
		var record passRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return nil, fmt.Errorf("invalid wallet pass record at line %d", line)
		}
		key, matches := strings.CutSuffix(record.Key, suffix)
		if !matches || !passObjectMatches(record.Object, key) || record.ExpiresAt.IsZero() {
			return nil, fmt.Errorf("invalid wallet pass record at line %d", line)
		}
		if previous, exists := passes.index[key]; exists && previous.Object != record.Object {
			return nil, fmt.Errorf("conflicting wallet pass at line %d", line)
		}
		passes.index[key] = record
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return passes, nil
}

func passObjectMatches(object, key string) bool {
	id, err := strconv.ParseInt(key, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != key {
		return false
	}
	owner, matches := strings.CutPrefix(object, "/passes/")
	if !matches {
		return false
	}
	owner, matches = strings.CutSuffix(owner, "/"+key+".pkpass")
	uid, err := strconv.ParseInt(owner, 10, 64)
	return matches && err == nil && uid > 0 && strconv.FormatInt(uid, 10) == owner
}

func (p *Passes) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	if p == nil {
		return nil, ErrPassNotFound
	}
	record, found := p.index[key]
	if !found || !time.Now().Before(record.ExpiresAt) {
		return nil, ErrPassNotFound
	}
	storage := p.storage
	region := cmp.Or(storage.Region, "ap-northeast-1")
	endpoint := cmp.Or(storage.Endpoint, "https://s3."+region+".amazonaws.com")
	objectURL, err := url.JoinPath(endpoint, storage.Bucket, strings.Trim(storage.Parent, "/"), strings.TrimLeft(record.Object, "/"))
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, objectURL, nil)
	if err != nil {
		return nil, err
	}
	empty := sha256.Sum256(nil)
	digest := hex.EncodeToString(empty[:])
	request.Header.Set("X-Amz-Content-Sha256", digest)
	credentials := aws.Credentials{AccessKeyID: storage.KeyID, SecretAccessKey: storage.Secret}
	if err := v4.NewSigner().SignHTTP(ctx, credentials, request, digest, "s3", region, time.Now().UTC(), func(o *v4.SignerOptions) {
		o.DisableURIPathEscaping = true
	}); err != nil {
		return nil, err
	}
	response, err := p.client.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode == http.StatusOK {
		return response.Body, nil
	}
	response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil, ErrPassNotFound
	}
	return nil, fmt.Errorf("wallet pass storage returned HTTP %d", response.StatusCode)
}
