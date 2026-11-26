package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"open_live_platform/api/platform/v1"
	"open_live_platform/internal/app"
	"open_live_platform/internal/auth"
	"open_live_platform/internal/etcdlive"
	"open_live_platform/internal/livebot"
)

func TestEtcdLubanReloadAndGRPCDiscovery(t *testing.T) {
	url := os.Getenv("OPEN_LIVE_TEST_ETCD_URL")
	if url == "" {
		t.Skip("set OPEN_LIVE_TEST_ETCD_URL to run real etcd integration")
	}
	cfg, db := postgresTestDatabase(t)
	ctx, stopTest := context.WithTimeout(t.Context(), 35*time.Second)
	defer stopTest()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	conn, err := clientv3.New(clientv3.Config{Endpoints: []string{url}, DialTimeout: 3 * time.Second})
	check(err)
	t.Cleanup(func() { conn.Close() })
	seed, err := app.New(ctx, cfg)
	check(err)
	t.Cleanup(seed.Close)
	account := func(name string) auth.Session {
		s, err := seed.Accounts.SignUp(ctx, name+"@etcd.test", "password", name)
		check(err)
		return s
	}
	anchor, bot := account("anchor"), account("robot")
	holders := make([]auth.Session, 4)
	for i := range holders {
		holders[i] = account(fmt.Sprintf("holder%d", i))
		_, err := db.Exec(ctx, `INSERT INTO nft_bindings(user_id,token_id,address) VALUES($1,$2,$3)`,
			holders[i].User.ID, i+101, "0x"+strings.Repeat("a", 40))
		check(err)
	}
	prefix := fmt.Sprintf("/integration/live/%d", time.Now().UnixNano())
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = conn.Delete(cleanup, prefix, clientv3.WithPrefix())
	})
	cfg.Etcd = etcdlive.Config{Endpoints: []string{url}, ConfigPrefix: prefix + "/config/", DiscoveryPrefix: prefix + "/discovery"}
	cfg.LubanRoomBots = true
	cfg.LubanCollectibles = true
	cfg.RoomBots = livebot.Config{RoomPopulation: 20, StaySeconds: 100, MinimumGiftPoints: 100}
	version := prefix + "/config/version"
	name := func(table string) string { return prefix + "/config/json/" + table + ".json" }
	welcome := name("robot_welcome_message_text")
	rights := name("nft_equity_data")
	_, err = conn.Txn(ctx).Then(
		clientv3.OpPut(name("robot_create_property"), fmt.Sprintf(`[{"id":1,"uid":%d,"location":"ja"}]`, bot.User.ID)),
		clientv3.OpPut(welcome, `[{"id":1,"weight":"ja","population":1,"text":"first"}]`),
		clientv3.OpPut(name("robot_interactive_gift_text"), `[]`),
		clientv3.OpPut(name("robot_room_entry_time"), `[{"id":1,"weight":1,"time":0}]`),
		clientv3.OpPut(name("robot_welcome_message_time"), `[{"id":1,"weight":1,"time":0}]`),
		clientv3.OpPut(rights, `[{"id":11,"rights_name":"Privilege A"}]`),
		clientv3.OpPut(name("live_decoration_config"), `[{"id":1,"outfit_name":"Rose","unlock_condition":1,"exclusive_medal":"rose"}]`),
		clientv3.OpPut(name("nft_card"), `[]`),
		clientv3.OpPut(version, "1"),
	).Commit()
	check(err)
	listenAddress := func() string {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		check(err)
		defer l.Close()
		return l.Addr().String()
	}
	type instance struct {
		platform *app.App
		stop     context.CancelFunc
		done     chan error
	}
	launch := func() instance {
		options := cfg
		options.Addr, options.GRPCAddr, options.MetricsAddr = listenAddress(), listenAddress(), listenAddress()
		p, err := app.New(ctx, options)
		check(err)
		runCtx, stop := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- p.Run(runCtx); close(done) }()
		t.Cleanup(func() {
			stop()
			if err := <-done; err != nil {
				t.Error(err)
			}
			p.Close()
		})
		return instance{p, stop, done}
	}
	a, b := launch(), launch()
	waitUntil := func(description string, condition func() bool) {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		for {
			if condition() {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", description)
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(60 * time.Millisecond):
			}
		}
	}
	setCount := func(want int) bool {
		got, err := conn.Get(ctx, cfg.Etcd.DiscoveryPrefix+"/"+etcdlive.PlatformService+"/", clientv3.WithPrefix())
		if err != nil || len(got.Kvs) != want {
			return false
		}
		for _, entry := range got.Kvs {
			if entry.Lease == 0 {
				return false
			}
		}
		return true
	}
	waitUntil("two leased gRPC nodes", func() bool { return setCount(2) })
	resolver := etcdlive.Discovery{Client: conn, Prefix: cfg.Etcd.DiscoveryPrefix}
	grpcConn, err := grpc.NewClient("etcd-grpc:///"+etcdlive.PlatformService,
		grpc.WithResolvers(resolver.Resolver()), grpc.WithTransportCredentials(insecure.NewCredentials()))
	check(err)
	t.Cleanup(func() { grpcConn.Close() })
	query, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	health, err := healthpb.NewHealthClient(grpcConn).Check(query, &healthpb.HealthCheckRequest{Service: etcdlive.PlatformService}, grpc.WaitForReady(true))
	check(err)
	if health.Status != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("gRPC health = %v", health.Status)
	}
	stats, err := platformv1.NewPlatformServiceClient(grpcConn).GetStats(query, &platformv1.GetStatsRequest{}, grpc.WaitForReady(true))
	check(err)
	if stats.Users < 6 {
		t.Fatalf("gRPC stats via discovery = %+v", stats)
	}

	room, err := a.platform.Rooms.Start(ctx, anchor.User.ID, "Bots", "")
	check(err)
	events, unsubscribe, err := b.platform.Rooms.Subscribe(ctx, room.ID, anchor.User.ID)
	check(err)
	defer unsubscribe()
	waitUntil("livebot joining from Luban configuration", func() bool {
		members, err := a.platform.Rooms.Members(ctx, room.ID)
		return err == nil && len(members) == 1 && members[0] == bot.User.ID
	})
	expectBot := func(who auth.Session, phrase string) {
		t.Helper()
		check(a.platform.Rooms.Join(ctx, room.ID, who.User.ID, ""))
		timeout := time.After(6 * time.Second)
		for {
			select {
			case e, ok := <-events:
				if !ok {
					t.Fatal("room stream closed")
				}
				if e.Type == "chat" && e.UserID == bot.User.ID {
					if e.Text != phrase {
						t.Fatalf("livebot said %q, want %q", e.Text, phrase)
					}
					return
				}
			case <-timeout:
				t.Fatalf("missing livebot chat %q", phrase)
			}
		}
	}
	expectBot(holders[0], "first")
	_, err = conn.Put(ctx, welcome, `[{"id":1,"weight":"ja","population":1,"text":"second"}]`)
	check(err)
	_, err = conn.Put(ctx, rights, `[{"id":11,"rights_name":"Privilege B"}]`)
	check(err)
	expectBot(holders[1], "first")
	_, err = conn.Put(ctx, version, "2")
	check(err)
	httpClient := &http.Client{Timeout: 2 * time.Second}
	get := func(addr, path string) string {
		response, err := httpClient.Get("http://" + addr + path)
		if err != nil {
			return ""
		}
		defer response.Body.Close()
		raw, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != 200 {
			return ""
		}
		return string(raw)
	}
	privileges := func() string {
		r, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+a.platform.Config.Addr+"/api/nft/creation_card/get_privilege_list", nil)
		check(err)
		r.Header.Set("Authorization", "Bearer "+anchor.Token)
		res, err := httpClient.Do(r)
		check(err)
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		check(err)
		if res.StatusCode != 200 {
			t.Fatalf("NFT privileges HTTP %d: %s", res.StatusCode, body)
		}
		return string(body)
	}
	waitUntil("accepted livebot revision", func() bool {
		return strings.Contains(get(a.platform.Config.MetricsAddr, "/metrics"), `platform_etcd_config_reload_total{result="accepted"} 1`)
	})
	expectBot(holders[2], "second")
	if body := privileges(); !strings.Contains(body, "Privilege B") {
		t.Fatalf("NFT privileges not reloaded: %s", body)
	}
	_, err = conn.Txn(ctx).Then(clientv3.OpPut(welcome, `{"bad":true}`), clientv3.OpPut(rights, `[{"id":11,"rights_name":"Privilege C"}]`),
		clientv3.OpPut(version, "3")).Commit()
	check(err)
	waitUntil("rejected invalid revision", func() bool {
		return strings.Contains(get(a.platform.Config.MetricsAddr, "/metrics"), `platform_etcd_config_reload_total{result="rejected"} 1`)
	})
	expectBot(holders[3], "second")
	if body := privileges(); !strings.Contains(body, "Privilege B") {
		t.Fatalf("invalid release displaced NFT privileges: %s", body)
	}
	if body := get(a.platform.Config.Addr, "/health"); body == "" {
		t.Fatal("health HTTP request failed")
	}
	metrics := get(a.platform.Config.MetricsAddr, "/metrics")
	if !strings.Contains(metrics, `http_requests_total{method="GET",path="/health"} 1`) ||
		!strings.Contains(metrics, "platform_etcd_discovery_registered 1") {
		t.Fatal("prometheus HTTP and lease metrics absent")
	}
	qr, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+a.platform.Config.Addr+"/api/device/qrcode", nil)
	check(err)
	qr.Header.Set("Authorization", "Bearer "+anchor.Token)
	qrResponse, err := httpClient.Do(qr)
	check(err)
	var ticket struct{ URL string }
	check(json.NewDecoder(qrResponse.Body).Decode(&ticket))
	qrResponse.Body.Close()
	dialer := websocket.Dialer{Subprotocols: []string{"h5relayer"}, HandshakeTimeout: 4 * time.Second}
	socket, _, err := dialer.DialContext(ctx, ticket.URL+fmt.Sprintf("?room_id=%d", room.ID), nil)
	check(err)
	t.Cleanup(func() { socket.Close() })
	check(socket.SetReadDeadline(time.Now().Add(4 * time.Second)))
	var greeting struct{ Content string }
	check(socket.ReadJSON(&greeting))
	waitUntil("WebSocket gauge", func() bool {
		body := get(a.platform.Config.MetricsAddr, "/metrics")
		return strings.Contains(body, "\nonlines 1") && strings.Contains(body, `status="101"`)
	})
	check(socket.WriteJSON(map[string]any{"content": "live_chat", "payload": []any{map[string]any{
		"message": map[string]any{"message": "metrics chat"},
	}}}))
	waitUntil("relay frame counter", func() bool {
		return strings.Contains(get(a.platform.Config.MetricsAddr, "/metrics"), `agent_msg_total{protocol="h5relayer"} 1`)
	})
	check(socket.Close())
	waitUntil("WebSocket disconnected gauge", func() bool { return strings.Contains(get(a.platform.Config.MetricsAddr, "/metrics"), "\nonlines 0") })
	a.stop()
	waitUntil("removal of stopped node", func() bool { return setCount(1) })
	query2, cancel2 := context.WithTimeout(ctx, 5*time.Second)
	defer cancel2()
	_, err = platformv1.NewPlatformServiceClient(grpcConn).GetStats(query2, &platformv1.GetStatsRequest{}, grpc.WaitForReady(true))
	check(err)
	b.stop()
	waitUntil("graceful unregister", func() bool { return setCount(0) })
}
