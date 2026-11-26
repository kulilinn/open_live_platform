package etcdlive

import (
	"context"
	"crypto/rand"
	"errors"
	"net"
	"sort"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	_ "google.golang.org/grpc/balancer/roundrobin"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/serviceconfig"
)

const PlatformService = "platform.v1.PlatformService"

type Discovery struct {
	Client *clientv3.Client
	Prefix string
	TTL    int64
}

func (d Discovery) keys(service string) string {
	return strings.TrimRight(d.Prefix, "/") + "/" + service + "/"
}

// Register publishes one gRPC endpoint for as long as its listener is available.
func (d Discovery) Register(ctx context.Context, address string, onState func(bool, error)) {
	key := d.keys(PlatformService) + rand.Text()
	ttl := d.TTL
	if ttl <= 0 {
		ttl = 30
	}
	for ctx.Err() == nil {
		err := d.keepRegistered(ctx, key, address, ttl, onState)
		if ctx.Err() == nil {
			onState(false, err)
		}
		if !pause(ctx, time.Second) {
			return
		}
	}
}

func (d Discovery) keepRegistered(ctx context.Context, key, address string, ttl int64, onState func(bool, error)) error {
	try, cancel := context.WithTimeout(ctx, 5*time.Second)
	lease, err := d.Client.Grant(try, ttl)
	if err != nil {
		cancel()
		return err
	}
	defer func() {
		cleanup, end := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer end()
		_, _ = d.Client.Revoke(cleanup, lease.ID)
	}()
	_, err = d.Client.Put(try, key, address, clientv3.WithLease(lease.ID))
	cancel()
	if err != nil {
		return err
	}
	replies, err := d.Client.KeepAlive(ctx, lease.ID)
	if err != nil {
		return err
	}
	onState(true, nil)
	for value := range replies {
		if value == nil || value.TTL <= 0 {
			break
		}
	}
	return errors.New("etcd service lease expired")
}

func (d Discovery) Resolver() resolver.Builder { return discoveryResolver{d} }

type discoveryResolver struct{ Discovery }

func (discoveryResolver) Scheme() string { return "etcd-grpc" }

func (d discoveryResolver) Build(target resolver.Target, cc resolver.ClientConn, _ resolver.BuildOptions) (resolver.Resolver, error) {
	service := target.Endpoint()
	if service == "" || strings.Contains(service, "/") {
		return nil, errors.New("invalid etcd gRPC target")
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &resolverWatch{cancel: cancel, rescan: make(chan struct{}, 1), done: make(chan struct{})}
	go func() { defer close(r.done); d.watchEndpoints(ctx, d.keys(service), cc, r.rescan) }()
	return r, nil
}

type resolverWatch struct {
	cancel context.CancelFunc
	rescan chan struct{}
	done   chan struct{}
}

func (r *resolverWatch) ResolveNow(resolver.ResolveNowOptions) {
	select {
	case r.rescan <- struct{}{}:
	default:
	}
}
func (r *resolverWatch) Close() { r.cancel(); <-r.done }

func (d discoveryResolver) watchEndpoints(ctx context.Context, key string, cc resolver.ClientConn, rescan <-chan struct{}) {
	serviceConfig := cc.ParseServiceConfig(`{"loadBalancingConfig":[{"round_robin":{}}]}`)
	for ctx.Err() == nil {
		query, cancel := context.WithTimeout(ctx, 5*time.Second)
		snapshot, err := d.Client.Get(query, key, clientv3.WithPrefix())
		cancel()
		if err != nil {
			cc.ReportError(err)
			if !pause(ctx, time.Second) {
				return
			}
			continue
		}
		nodes := make(map[string]string)
		for _, item := range snapshot.Kvs {
			nodes[string(item.Key)] = string(item.Value)
		}
		updateEndpoints(cc, serviceConfig, nodes)
		watchCtx, stop := context.WithCancel(ctx)
		events := d.Client.Watch(watchCtx, key, clientv3.WithPrefix(), clientv3.WithRev(snapshot.Header.Revision+1))
	watch:
		for {
			select {
			case <-ctx.Done():
				stop()
				return
			case <-rescan:
				break watch
			case response, open := <-events:
				if !open {
					break watch
				}
				if err := response.Err(); err != nil {
					cc.ReportError(err)
					break watch
				}
				for _, event := range response.Events {
					if event.Type == clientv3.EventTypeDelete {
						delete(nodes, string(event.Kv.Key))
					} else {
						nodes[string(event.Kv.Key)] = string(event.Kv.Value)
					}
				}
				updateEndpoints(cc, serviceConfig, nodes)
			}
		}
		stop()
		if !pause(ctx, 100*time.Millisecond) {
			return
		}
	}
}

func updateEndpoints(cc resolver.ClientConn, config *serviceconfig.ParseResult, nodes map[string]string) {
	seen := make(map[string]bool, len(nodes))
	addresses := make([]resolver.Address, 0, len(nodes))
	for _, value := range nodes {
		host, port, err := net.SplitHostPort(value)
		if err != nil || host == "" || port == "0" || seen[value] {
			continue
		}
		seen[value] = true
		addresses = append(addresses, resolver.Address{Addr: value})
	}
	sort.Slice(addresses, func(i, j int) bool { return addresses[i].Addr < addresses[j].Addr })
	_ = cc.UpdateState(resolver.State{Addresses: addresses, ServiceConfig: config})
}
