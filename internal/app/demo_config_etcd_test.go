package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"open_live_platform/internal/admin"
	"open_live_platform/internal/app"
	"open_live_platform/internal/etcdlive"
)

func TestEtcdDemoConfigSharedSnapshots(t *testing.T) {
	etcdURL := os.Getenv("OPEN_LIVE_TEST_ETCD_URL")
	if etcdURL == "" {
		t.Skip("set OPEN_LIVE_TEST_ETCD_URL to run real etcd integration")
	}
	cfg, _ := postgresTestDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	conn, err := clientv3.New(clientv3.Config{Endpoints: []string{etcdURL}, DialTimeout: 3 * time.Second})
	check(err)
	t.Cleanup(func() { conn.Close() })
	prefix := fmt.Sprintf("/integration/demo-%d/", time.Now().UnixNano())
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 2*time.Second)
		defer stop()
		_, _ = conn.Delete(cleanup, prefix, clientv3.WithPrefix())
	})
	version, items := prefix+"version", prefix+"json/demo_tbitem.json"
	put := func(key, value string) {
		t.Helper()
		_, err := conn.Put(ctx, key, value)
		check(err)
	}
	put(version, "0")
	cfg.Etcd = etcdlive.Config{Endpoints: []string{etcdURL}, ConfigPrefix: prefix}
	dir := t.TempDir()
	cfg.DemoItemsFile = filepath.Join(dir, "demo.json")
	check(os.WriteFile(cfg.DemoItemsFile, []byte(`[{"id":7,"name":"file fallback","count":3}]`), 0600))
	passwordHash, err := admin.SystemPassword("demo-config-test-password")
	check(err)
	staff, err := json.Marshal([]admin.Principal{
		{ID: 77, Area: admin.OperationArea, Tenant: admin.OperationTenant, Username: "config-fixture", Admin: true, PasswordHash: passwordHash},
	})
	check(err)
	cfg.AdminAccountsFile = filepath.Join(dir, "operators.json")
	check(os.WriteFile(cfg.AdminAccountsFile, staff, 0600))
	address := func() string {
		t.Helper()
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		check(err)
		defer listener.Close()
		return listener.Addr().String()
	}
	launch := func() *app.App {
		t.Helper()
		options := cfg
		options.Addr, options.AdminAddr, options.GRPCAddr, options.MetricsAddr = address(), address(), address(), address()
		instance, err := app.New(ctx, options)
		check(err)
		runCtx, stop := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- instance.Run(runCtx) }()
		t.Cleanup(func() {
			stop()
			if err := <-done; err != nil {
				t.Error(err)
			}
			instance.Close()
		})
		return instance
	}
	client := http.Client{Timeout: time.Second}
	read := func(address, path string) string {
		response, err := client.Get("http://" + address + path)
		if err != nil {
			return ""
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != http.StatusOK {
			return ""
		}
		return string(body)
	}
	until := func(description string, ok func() bool) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		ticker := time.NewTicker(30 * time.Millisecond)
		defer ticker.Stop()
		for !ok() {
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-deadline:
				t.Fatalf("timed out waiting for %s", description)
			case <-ticker.C:
			}
		}
	}
	viewsMatch := func(instances []*app.App, required ...string) bool {
		first := ""
		for _, instance := range instances {
			for _, addr := range []string{instance.Config.Addr, instance.Config.AdminAddr} {
				body := read(addr, "/testconfig")
				if body == "" || (first != "" && first != body) {
					return false
				}
				first = body
			}
		}
		for _, want := range required {
			if !strings.Contains(first, want) {
				return false
			}
		}
		return true
	}
	a := launch()
	until("file fallback on the public and operator ports", func() bool { return viewsMatch([]*app.App{a}, `"len":1`, "file fallback") })
	put(items, `[{"id":42,"name":"Published first","desc":"test entry","count":4,"key":"demo.item"},{"id":42,"name":"Published second","desc":"latest map value","count":5,"key":"demo.replacement"}]`)
	b := launch()
	instances := []*app.App{a, b}
	until("both instances to keep the fallback while the table is staged", func() bool { return viewsMatch(instances, `"len":1`, "file fallback") })
	put(version, "1")
	until("both instances to publish one Luban view", func() bool {
		return viewsMatch(instances, `"len":2`, "Published first", "Published second", "demo.replacement")
	})
	var response struct{ Data struct{ List, Map string } }
	check(json.Unmarshal([]byte(read(a.Config.Addr, "/testconfig")), &response))
	if strings.Contains(response.Data.Map, "Published first") || !strings.Contains(response.Data.Map, "Published second") ||
		!strings.Contains(response.Data.List, "Published first") {
		t.Fatalf("Luban list order and last-value map differ: %+v", response.Data)
	}
	_, err = conn.Delete(ctx, items)
	check(err)
	cold := launch()
	instances = append(instances, cold)
	until("a new instance to read the published table while its deletion is staged", func() bool {
		return viewsMatch(instances, `"len":2`, "Published first", "Published second")
	})
	keepsOld := func(rejections int) {
		t.Helper()
		until("all instances to reject the bad revision", func() bool {
			want := fmt.Sprintf(`platform_etcd_config_reload_total{result="rejected"} %d`, rejections)
			for _, instance := range instances {
				if !strings.Contains(read(instance.Config.MetricsAddr, "/metrics"), want) {
					return false
				}
			}
			return true
		})
		if !viewsMatch(instances, `"len":2`, "Published first", "Published second") {
			t.Fatal("a rejected Luban revision changed the published view")
		}
	}
	put(items, `[{"id":42,"name":"missing count","desc":"invalid","Count":1,"key":"demo.bad"}]`)
	put(version, "2")
	keepsOld(1)
	_, err = conn.Delete(ctx, items)
	check(err)
	put(version, "3")
	keepsOld(2)
	put(items, `[]`)
	put(version, "4")
	until("both instances to publish the explicit empty table", func() bool { return viewsMatch(instances, `"data":{"len":0}`) })
}
