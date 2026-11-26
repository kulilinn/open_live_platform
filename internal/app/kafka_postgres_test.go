package app_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
	"open_live_platform/internal/app"
	"open_live_platform/internal/edge"
	"open_live_platform/internal/relay"
)

func TestPostgresKafkaNoticeReachesRemoteH5Session(t *testing.T) {
	cfg, _ := postgresTestDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	prefix := "livemedia-test-" + strings.ToLower(rand.Text())
	quick, late := prefix+"-now", prefix+"-delayed"
	var brokers []string
	if configured := os.Getenv("OPEN_LIVE_TEST_KAFKA_BROKERS"); configured != "" {
		brokers = strings.Split(configured, ",")
	} else {
		broker := kfake.MustCluster(kfake.NumBrokers(1))
		t.Cleanup(broker.Close)
		brokers = broker.ListenAddrs()
	}
	producer, err := kgo.NewClient(kgo.SeedBrokers(brokers...))
	check(err)
	t.Cleanup(producer.Close)
	create := kmsg.NewPtrCreateTopicsRequest()
	for _, name := range []string{quick, late} {
		topic := kmsg.NewCreateTopicsRequestTopic()
		topic.Topic, topic.NumPartitions, topic.ReplicationFactor = name, 1, 1
		create.Topics = append(create.Topics, topic)
	}
	created, err := create.RequestWith(ctx, producer)
	check(err)
	for _, topic := range created.Topics {
		if topic.ErrorCode != 0 {
			t.Fatalf("create test topic %s: Kafka error %d", topic.Topic, topic.ErrorCode)
		}
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		remove := kmsg.NewPtrDeleteTopicsRequest()
		remove.TopicNames = []string{quick, late}
		for _, name := range remove.TopicNames {
			remove.Topics = append(remove.Topics, kmsg.DeleteTopicsRequestTopic{Topic: &name})
		}
		deleted, err := remove.RequestWith(cleanup, producer)
		if err != nil {
			t.Error(err)
			return
		}
		for _, topic := range deleted.Topics {
			if topic.ErrorCode != 0 {
				t.Errorf("remove test topic: Kafka error %d", topic.ErrorCode)
			}
		}
	})
	cfg.RelayRust = relay.RustOptions{AppKey: "kafka-notice-test", AppSecret: "local-test-secret"}
	watcher, err := app.New(ctx, cfg)
	check(err)
	t.Cleanup(watcher.Close)
	actor, err := watcher.Accounts.SignUp(ctx, "recipient@edge.test", "password", "Recipient")
	check(err)
	stranger, err := watcher.Accounts.SignUp(ctx, "stranger@edge.test", "password", "Stranger")
	check(err)
	server := httptest.NewServer(watcher.Server.Handler)
	t.Cleanup(server.Close)
	paired := openPrivateH5(t, server, actor)
	t.Cleanup(func() { paired.Close() })
	isolation := openPrivateH5(t, server, stranger)
	t.Cleanup(func() { isolation.Close() })
	reserveAddr := func() string {
		x, err := net.Listen("tcp", "127.0.0.1:0")
		check(err)
		defer x.Close()
		return x.Addr().String()
	}
	cfg.Addr, cfg.GRPCAddr = reserveAddr(), reserveAddr()
	cfg.Kafka = edge.KafkaConfig{Brokers: brokers, Topics: []edge.NoticeTopic{
		{Name: quick, Group: "test-realtime", TTL: time.Minute}, {Name: late, Group: "test-delay", TTL: 2 * time.Minute},
	}}
	consumer, err := app.New(ctx, cfg)
	check(err)
	t.Cleanup(consumer.Close)
	done := make(chan error, 1)
	go func() { done <- consumer.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Kafka consumer runtime: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Kafka consumer runtime did not shut down")
		}
	})
	makeNotice := func(topic, text string, created time.Time) *kgo.Record {
		return &kgo.Record{Topic: topic, Value: []byte(fmt.Sprintf(`{"userId":"%d","noticeTarget":"Fans","noticeType":1,"content":{"message":%q}}`, actor.User.ID, text)),
			Headers: []kgo.RecordHeader{{Key: "x-msg-generation-time", Value: []byte(strconv.FormatInt(created.UnixMilli(), 10))}}}
	}
	now := time.Now()
	current, delayed := makeNotice(quick, "active", now), makeNotice(late, "within delay TTL", now.Add(-90*time.Second))
	expired := makeNotice(quick, "expired", now.Add(-90*time.Second))
	check(producer.ProduceSync(ctx, expired, current, delayed).FirstErr())
	wanted := map[string]bool{string(current.Value): true, string(delayed.Value): true}
	check(paired.SetReadDeadline(time.Now().Add(12 * time.Second)))
	for len(wanted) > 0 {
		var frame struct{ Content, Queue, Payload string }
		_, data, err := paired.ReadMessage()
		check(err)
		var envelope struct{ Content string }
		check(json.Unmarshal(data, &envelope))
		if envelope.Content != "notice" {
			continue
		}
		check(json.Unmarshal(data, &frame))
		if frame.Queue != strconv.FormatInt(actor.User.ID, 10) || !wanted[frame.Payload] {
			t.Fatalf("unexpected Kafka frame %s", data)
		}
		delete(wanted, frame.Payload)
	}
	check(isolation.SetReadDeadline(time.Now().Add(250 * time.Millisecond)))
	for {
		_, data, err := isolation.ReadMessage()
		if err != nil {
			break
		}
		var envelope struct{ Content string }
		check(json.Unmarshal(data, &envelope))
		if envelope.Content == "notice" {
			t.Fatalf("targeted Kafka notice reached an unrelated user: %s", data)
		}
	}
}
