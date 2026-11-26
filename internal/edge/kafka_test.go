package edge

import (
	"context"
	"errors"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
)

func TestKafkaNoticeRetryAndExpiry(t *testing.T) {
	const topic = "notices"
	broker := kfake.MustCluster(kfake.NumBrokers(1), kfake.SeedTopics(1, topic))
	defer broker.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	client, err := kgo.NewClient(kgo.SeedBrokers(broker.ListenAddrs()...))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	const body = `{"userId":"9007199254740993","noticeTarget":"Fans","noticeType":1,"content":{"message":"live"},"createTime":"2025-01-01T00:00:00Z"}`
	expired := kgo.RecordHeader{Key: "x-msg-generation-time", Value: []byte(strconv.FormatInt(time.Now().Add(-time.Hour).UnixMilli(), 10))}
	if err := client.ProduceSync(ctx, &kgo.Record{Topic: topic, Value: []byte(body), Headers: []kgo.RecordHeader{expired}},
		&kgo.Record{Topic: topic, Value: []byte("invalid")}, &kgo.Record{Topic: topic, Value: []byte(body)}).FirstErr(); err != nil {
		t.Fatal(err)
	}
	var attempts atomic.Int32
	delivered := make(chan bool, 1)
	done := make(chan error, 1)
	go func() {
		done <- ConsumeNotices(ctx, KafkaConfig{Brokers: broker.ListenAddrs(), Topics: []NoticeTopic{{topic, "test-group", time.Minute}}},
			func(_ context.Context, uid int64, kind string, data any) error {
				if attempts.Add(1) == 1 {
					return errors.New("database connection interrupted")
				}
				delivered <- uid == 9007199254740993 && kind == "notice" && data == body
				return nil
			})
	}()
	select {
	case ok := <-delivered:
		if !ok {
			t.Error("notification payload or recipient changed")
		}
	case <-ctx.Done():
		t.Error("timed out waiting for Kafka notice")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 2 {
		t.Fatalf("delivery attempts = %d", attempts.Load())
	}
}
