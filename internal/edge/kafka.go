package edge

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"golang.org/x/sync/errgroup"
	"open_live_platform/internal/relay"
)

type NoticeTopic struct {
	Name, Group string
	TTL         time.Duration
}

type KafkaConfig struct {
	Brokers []string
	TLS     bool
	Topics  []NoticeTopic
}

// ConsumeNotices runs one consumer group per configured LiveMedia topic.
// Rust's producer persists the inbox row separately. The edge only delivers it.
func ConsumeNotices(ctx context.Context, cfg KafkaConfig, push relay.PushPrivate) error {
	group, ctx := errgroup.WithContext(ctx)
	for _, topic := range cfg.Topics {
		group.Go(func() error {
			if topic.Name == "" || topic.Group == "" || len(cfg.Brokers) == 0 {
				return errors.New("Kafka notice topic, group and broker required")
			}
			opts := []kgo.Opt{kgo.SeedBrokers(cfg.Brokers...), kgo.ConsumerGroup(topic.Group), kgo.ConsumeTopics(topic.Name),
				kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()), kgo.DisableAutoCommit(), kgo.BlockRebalanceOnPoll()}
			if cfg.TLS {
				opts = append(opts, kgo.DialTLSConfig(&tls.Config{MinVersion: tls.VersionTLS12}))
			}
			for ctx.Err() == nil {
				client, err := kgo.NewClient(opts...)
				if err != nil {
					return err
				}
				err = consumeGroup(ctx, client, topic, push)
				client.CloseAllowingRebalance()
				if ctx.Err() != nil {
					break
				}
				log.Printf("restarting Kafka notice group %s: %v", topic.Group, err)
				select {
				case <-ctx.Done():
				case <-time.After(time.Second):
				}
			}
			return nil
		})
	}
	return group.Wait()
}

func consumeGroup(ctx context.Context, client *kgo.Client, topic NoticeTopic, push relay.PushPrivate) error {
	for ctx.Err() == nil {
		fetches := client.PollRecords(ctx, 64)
		if errs := fetches.Errors(); len(errs) > 0 {
			return errs[0].Err
		}
		records := fetches.Records()
		for _, record := range records {
			if err := deliverNotice(ctx, record, topic.TTL, time.Now(), push); err != nil {
				return fmt.Errorf("offset %d: %w", record.Offset, err)
			}
		}
		if len(records) > 0 {
			if err := client.CommitRecords(ctx, records...); err != nil {
				return err
			}
		}
		client.AllowRebalance()
	}
	return ctx.Err()
}

func deliverNotice(ctx context.Context, r *kgo.Record, ttl time.Duration, now time.Time, push relay.PushPrivate) error {
	for _, h := range r.Headers {
		if h.Key == "x-msg-generation-time" {
			millis, err := strconv.ParseInt(string(h.Value), 10, 64)
			if err == nil && now.Sub(time.UnixMilli(millis)) > ttl {
				return nil
			}
		}
	}
	var msg struct {
		UserID int64  `json:"userId,string"`
		Target string `json:"noticeTarget"`
		Type   int    `json:"noticeType"`
	}
	if err := json.Unmarshal(r.Value, &msg); err != nil || msg.UserID < 1 || msg.Type < 1 || msg.Type > 34 || (msg.Target != "Anchor" && msg.Target != "Fans" && msg.Target != "User") {
		log.Printf("skip malformed Kafka notice %s/%d/%d", r.Topic, r.Partition, r.Offset)
		return nil
	}
	return push(ctx, msg.UserID, "notice", string(r.Value))
}
