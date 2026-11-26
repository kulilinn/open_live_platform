package etcdlive

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
)

type Config struct {
	Endpoints          []string
	Username, Password string
	ConfigPrefix       string
	DiscoveryPrefix    string
	AdvertiseAddress   string
}

func FromEnv() Config {
	urls := strings.FieldsFunc(os.Getenv("ETCD_URL"), func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' })
	return Config{Endpoints: urls, Username: os.Getenv("ETCD_USERNAME"), Password: os.Getenv("ETCD_PASSWORD"),
		ConfigPrefix: os.Getenv("ETCD_CONFIG_PREFIX"), DiscoveryPrefix: os.Getenv("PLATFORM_DISCOVERY_PREFIX"),
		AdvertiseAddress: os.Getenv("PLATFORM_DISCOVERY_ADDR")}
}

func (c Config) Open() (*clientv3.Client, error) {
	if len(c.Endpoints) == 0 {
		return nil, errors.New("ETCD_URL is required")
	}
	return clientv3.New(clientv3.Config{Endpoints: c.Endpoints, Username: c.Username, Password: c.Password, DialTimeout: 5 * time.Second})
}

type Tables struct {
	Client *clientv3.Client
	Prefix string
	Names  []string
}

func (s Tables) versionKey() string {
	return strings.TrimRight(s.Prefix, "/") + "/version"
}

// Read obtains the version marker and the supported Luban tables at one revision.
func (s Tables) Read(ctx context.Context, revision int64) (map[string][]byte, int64, error) {
	prefix := strings.TrimRight(s.Prefix, "/")
	ops := make([]clientv3.Op, 0, len(s.Names)+1)
	ops = append(ops, clientv3.OpGet(s.versionKey(), clientv3.WithRev(revision)))
	for _, name := range s.Names {
		ops = append(ops, clientv3.OpGet(prefix+"/json/"+name+".json", clientv3.WithRev(revision)))
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	result, err := s.Client.Txn(ctx).Then(ops...).Commit()
	if err != nil {
		return nil, 0, err
	}
	version := result.Responses[0].GetResponseRange().Kvs
	if len(version) == 0 {
		return nil, result.Header.Revision, errors.New("etcd configuration version key missing")
	}
	tables := make(map[string][]byte, len(s.Names))
	var pending, missing bool
	for i, name := range s.Names {
		rows := result.Responses[i+1].GetResponseRange()
		if len(rows.Kvs) == 0 {
			missing = true
			continue
		}
		pending = pending || rows.Kvs[0].ModRevision > version[0].ModRevision
		tables[name] = rows.Kvs[0].Value
	}
	if revision == 0 && (pending || missing) && version[0].ModRevision < result.Header.Revision {
		// A staged deletion has no current ModRevision, so a missing table can
		// also require a read at the published revision.
		published, _, err := s.Read(ctx, version[0].ModRevision)
		if err == nil || pending || !errors.Is(err, rpctypes.ErrCompacted) {
			return published, result.Header.Revision, err
		}
	}
	return tables, result.Header.Revision, nil
}

// Watch applies complete, validated snapshots only after a version publish.
// A bad publish leaves the previous configuration in use.
func (s Tables) Watch(ctx context.Context, revision int64, apply func(map[string][]byte) error, onError func(error)) {
	for ctx.Err() == nil {
		watchCtx, cancel := context.WithCancel(ctx)
		for response := range s.Client.Watch(watchCtx, s.versionKey(), clientv3.WithRev(revision+1)) {
			if response.Err() != nil {
				onError(response.Err())
				break
			}
			for _, event := range response.Events {
				revision = event.Kv.ModRevision
				if event.Type == clientv3.EventTypeDelete {
					continue
				}
				tables, _, err := s.Read(ctx, revision)
				if err == nil {
					err = apply(tables)
				}
				if err != nil {
					onError(fmt.Errorf("publish revision %d: %w", revision, err))
				}
			}
		}
		cancel()
		if !pause(ctx, time.Second) {
			return
		}
		// A canceled watch may have fallen behind etcd's compaction revision.
		tables, next, err := s.Read(ctx, 0)
		if next > 0 {
			revision = next
		}
		if err == nil {
			err = apply(tables)
		}
		if err != nil {
			onError(err)
		}
	}
}

func pause(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
