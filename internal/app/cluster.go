package app

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync"

	clientv3 "go.etcd.io/etcd/client/v3"
	"open_live_platform/internal/collectible"
	"open_live_platform/internal/etcdlive"
	"open_live_platform/internal/livebot"
)

type appCluster struct {
	client    *clientv3.Client
	tables    *etcdlive.Tables
	revision  int64
	botBase   livebot.Config
	nft       *collectible.LubanCatalog
	usesBots  bool
	usesNFT   bool
	discovery *etcdlive.Discovery
}

func initCluster(ctx context.Context, config *Config) (*appCluster, error) {
	ec := config.Etcd
	prefix := ec.DiscoveryPrefix
	if prefix == "" && ec.AdvertiseAddress != "" {
		name := os.Getenv("ZENOH_CLUSTER_NAME")
		if name == "" {
			name = "LRPRD"
		}
		prefix = "/@grpc-" + name
	}
	if !config.LubanRoomBots && !config.LubanCollectibles && prefix == "" {
		return nil, nil
	}
	conn, err := ec.Open()
	if err != nil {
		return nil, err
	}
	result := &appCluster{client: conn, botBase: config.RoomBots, usesBots: config.LubanRoomBots, usesNFT: config.LubanCollectibles}
	if prefix != "" {
		result.discovery = &etcdlive.Discovery{Client: conn, Prefix: prefix}
	}
	if result.usesBots || result.usesNFT {
		if strings.TrimRight(ec.ConfigPrefix, "/") == "" {
			conn.Close()
			return nil, fmt.Errorf("ETCD_CONFIG_PREFIX is required for Luban")
		}
		var names []string
		if result.usesBots {
			names = append(names, livebot.LubanTables...)
		}
		if result.usesNFT {
			names = append(names, collectible.LubanTables...)
		}
		tables := etcdlive.Tables{Client: conn, Prefix: ec.ConfigPrefix, Names: names}
		data, rev, err := tables.Read(ctx, 0)
		if err == nil {
			config.RoomBots, result.nft, err = result.decode(data)
		}
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("load Luban tables: %w", err)
		}
		result.tables, result.revision = &tables, rev
	}
	return result, nil
}

func (c *appCluster) decode(data map[string][]byte) (livebot.Config, *collectible.LubanCatalog, error) {
	bots := c.botBase
	var nft *collectible.LubanCatalog
	var err error
	if c.usesBots {
		bots, err = livebot.FromLuban(c.botBase, data)
		if err != nil {
			return bots, nil, err
		}
	}
	if c.usesNFT {
		nft, err = collectible.ReadLubanCatalog(data)
	}
	return bots, nft, err
}

func (c *appCluster) close() {
	if c != nil {
		_ = c.client.Close()
	}
}

func metricsAddress() string {
	if addr := os.Getenv("METRICS_ADDR"); addr != "" {
		return addr
	}
	if port := os.Getenv("METRICS_PORT"); port != "" {
		return net.JoinHostPort("0.0.0.0", port)
	}
	return ""
}

func (a *App) monitorCluster(ctx context.Context, listener net.Listener) *sync.WaitGroup {
	done := new(sync.WaitGroup)
	if a.cluster == nil {
		return done
	}
	if a.cluster.tables != nil {
		done.Add(1)
		go func() {
			defer done.Done()
			a.cluster.tables.Watch(ctx, a.cluster.revision, func(data map[string][]byte) error {
				bots, nft, err := a.cluster.decode(data)
				if err == nil && a.cluster.usesBots {
					err = a.db.ConfigureRoomBots(bots)
				}
				if err == nil && a.cluster.usesNFT {
					a.collectibleConfig.ConfigureLuban(nft)
				}
				if err == nil && a.metrics != nil {
					a.metrics.Reloads.WithLabelValues("accepted").Inc()
				}
				return err
			}, func(err error) {
				if ctx.Err() == nil {
					if a.metrics != nil {
						a.metrics.Reloads.WithLabelValues("rejected").Inc()
					}
					log.Printf("reload Luban tables: %v", err)
				}
			})
		}()
	}
	if discovery := a.cluster.discovery; discovery != nil {
		done.Add(1)
		go func() {
			defer done.Done()
			defer func() {
				if a.metrics != nil {
					a.metrics.Discovery.Set(0)
				}
			}()
			address, err := advertisedAddress(a.Config.Etcd.AdvertiseAddress, listener.Addr().String())
			if err != nil {
				log.Printf("publish gRPC node: %v", err)
				return
			}
			discovery.Register(ctx, address, func(ready bool, err error) {
				if a.metrics != nil {
					value := 0.0
					if ready {
						value = 1
					}
					a.metrics.Discovery.Set(value)
				}
				if err != nil {
					log.Printf("publish gRPC node: %v", err)
				}
			})
		}()
	}
	return done
}

func advertisedAddress(configured, actual string) (string, error) {
	host, port, err := net.SplitHostPort(actual)
	if err != nil {
		return "", err
	}
	if configured != "" {
		cfgHost, cfgPort, err := net.SplitHostPort(configured)
		if err != nil {
			return "", err
		}
		host = cfgHost
		if cfgPort != "0" {
			port = cfgPort
		}
	}
	address := net.ParseIP(host)
	if host == "" || address != nil && address.IsUnspecified() {
		return "", fmt.Errorf("set PLATFORM_DISCOVERY_ADDR to a reachable host and port")
	}
	return net.JoinHostPort(host, port), nil
}
