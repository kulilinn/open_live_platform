package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"open_live_platform/internal/posts"
	"open_live_platform/internal/store/postgres"
	"open_live_platform/internal/subscription"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context) error {
	sourceDSN, targetDSN := os.Getenv("LIVEMEDIA_MYSQL_DSN"), os.Getenv("DATABASE_URL")
	if sourceDSN == "" || targetDSN == "" {
		return errors.New("set LIVEMEDIA_MYSQL_DSN and DATABASE_URL")
	}
	config, err := mysql.ParseDSN(sourceDSN)
	if err != nil {
		return errors.New("invalid LIVEMEDIA_MYSQL_DSN")
	}
	config.ParseTime, config.Loc = true, time.UTC
	connector, err := mysql.NewConnector(config)
	if err != nil {
		return errors.New("invalid LIVEMEDIA_MYSQL_DSN")
	}
	source := sql.OpenDB(connector)
	defer source.Close()
	snapshot, err := source.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return fmt.Errorf("read MySQL snapshot: %w", err)
	}
	defer snapshot.Rollback()
	target, err := postgres.Open(ctx, targetDSN)
	if err != nil {
		return fmt.Errorf("open PostgreSQL: %w", err)
	}
	defer target.Shutdown()
	data := postgres.LegacyPublishing{
		Posts:        rowsFrom(ctx, snapshot, "anchor_post", postsQuery, readPost),
		Comments:     rowsFrom(ctx, snapshot, "anchor_post_comment", commentsQuery, readComment),
		PostLikes:    rowsFrom(ctx, snapshot, "post_like", postLikesQuery, readReaction),
		CommentLikes: rowsFrom(ctx, snapshot, "comment_like", commentLikesQuery, readReaction),
		Tips:         rowsFrom(ctx, snapshot, "post_reward_rank", tipsQuery, readReaction),
		Shares:       rowsFrom(ctx, snapshot, "share", sharesQuery, readShare),
		Groups:       rowsFrom(ctx, snapshot, "subscription_groups", groupsQuery, readGroup),
		Saved:        rowsFrom(ctx, snapshot, "subscription contact lists", savedQuery, readSaved),
	}
	counts, err := target.ImportLegacyPublishing(ctx, data)
	if err != nil {
		return err
	}
	fmt.Printf("posts=%d comments=%d post_likes=%d comment_likes=%d tips=%d unlocks=%d shares=%d groups=%d lists=%d existing=%d skipped=%d\n",
		counts.Posts, counts.Comments, counts.PostLikes, counts.CommentLikes, counts.Tips, counts.Unlocks, counts.Shares, counts.Groups, counts.Saved, counts.Existing, counts.Skipped)
	return nil
}

const postsQuery = "SELECT id,uid,content,level,is_del,only_text,price,url,create_at,img_num,video_num,video_length,buyer,tips,`reference`,pass_status,pass_at FROM anchor_post ORDER BY id"
const commentsQuery = `SELECT id,uid,post_id,content,is_del,create_at,parent_comment_id FROM anchor_post_comment ORDER BY id`
const postLikesQuery = `SELECT post_id,user_id,create_at,0 FROM (
	SELECT post_id,user_id,create_at,is_like,ROW_NUMBER() OVER (PARTITION BY post_id,user_id ORDER BY create_at DESC,is_like ASC) AS newest
	FROM post_like) history WHERE newest=1 AND is_like<>0 ORDER BY post_id,user_id`
const commentLikesQuery = `SELECT comment_id,user_id,create_at,0 FROM comment_like WHERE is_like<>0 ORDER BY comment_id,user_id`
const tipsQuery = `SELECT post_id,user_id,MAX(create_at),MAX(amount) FROM post_reward_rank
	WHERE amount>0 GROUP BY post_id,user_id ORDER BY post_id,user_id`
const sharesQuery = "SELECT id,uid,content,comments,photo,likes,status,create_at,update_at FROM `share` ORDER BY id"
const groupsQuery = `SELECT id,uid,group_name,anchors,ty,is_del FROM subscription_groups ORDER BY id`
const savedQuery = `SELECT uid,2,friend_uids FROM subscription_friend UNION ALL
	SELECT uid,3,anchors FROM subscription_blacklist ORDER BY 1,2`

func rowsFrom[T any](ctx context.Context, snapshot *sql.Tx, table, query string, read func(*sql.Rows) (T, error)) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		var zero T
		rows, err := snapshot.QueryContext(ctx, query)
		if err != nil {
			yield(zero, fmt.Errorf("read %s: %w", table, err))
			return
		}
		defer rows.Close()
		for rows.Next() {
			value, err := read(rows)
			if err != nil {
				yield(zero, fmt.Errorf("decode %s: %w", table, err))
				return
			}
			if !yield(value, nil) {
				return
			}
		}
		if err := rows.Err(); err != nil {
			yield(zero, fmt.Errorf("read %s: %w", table, err))
		}
	}
}

func readPost(rows *sql.Rows) (postgres.LegacyPost, error) {
	var row postgres.LegacyPost
	var deleted int
	var seenMedia, buyers string
	var reviewed sql.NullTime
	if err := rows.Scan(&row.ID, &row.OwnerID, &row.Content, &row.Level, &deleted, &row.OnlyText, &row.Price, &seenMedia,
		&row.CreatedAt, &row.Images, &row.Videos, &row.VideoLength, &buyers, &row.TipPoints, &row.Reference, &row.Status, &reviewed); err != nil {
		return row, err
	}
	row.Deleted = deleted != 0
	if reviewed.Valid {
		row.ReviewedAt = &reviewed.Time
	}
	if strings.TrimSpace(seenMedia) != "" && json.Unmarshal([]byte(seenMedia), &row.Media) != nil {
		return row, fmt.Errorf("post %d has invalid media JSON", row.ID)
	}
	if row.Media == nil {
		row.Media = []posts.Media{}
	}
	if row.Level == 0 && row.Price > 0 {
		var err error
		row.Buyers, err = uniqueIDs(strings.Split(buyers, ","))
		if err != nil {
			return row, fmt.Errorf("post %d has invalid buyer IDs", row.ID)
		}
	}
	return row, nil
}

func readComment(rows *sql.Rows) (posts.Comment, error) {
	var row posts.Comment
	var deleted int
	var parent sql.NullInt64
	if err := rows.Scan(&row.ID, &row.AuthorID, &row.PostID, &row.Content, &deleted, &row.CreatedAt, &parent); err != nil {
		return row, err
	}
	row.Deleted = deleted != 0
	if parent.Valid && parent.Int64 > 0 {
		row.ParentID = &parent.Int64
	}
	return row, nil
}

func readReaction(rows *sql.Rows) (postgres.LegacyPostReaction, error) {
	var row postgres.LegacyPostReaction
	err := rows.Scan(&row.TargetID, &row.ActorID, &row.CreatedAt, &row.Points)
	return row, err
}

func readShare(rows *sql.Rows) (postgres.LegacyShare, error) {
	var row postgres.LegacyShare
	err := rows.Scan(&row.ID, &row.OwnerID, &row.Content, &row.Comments, &row.Photo, &row.Likes, &row.Status, &row.CreatedAt, &row.UpdatedAt)
	return row, err
}

func readGroup(rows *sql.Rows) (postgres.LegacyGroup, error) {
	var row postgres.LegacyGroup
	var members string
	var deleted int
	if err := rows.Scan(&row.ID, &row.OwnerID, &row.Name, &members, &row.Kind, &deleted); err != nil {
		return row, err
	}
	row.Deleted = deleted != 0
	var err error
	if !row.Deleted {
		row.Members, err = parseMemberIDs(members)
	}
	return row, err
}

func readSaved(rows *sql.Rows) (postgres.LegacySavedUsers, error) {
	var row postgres.LegacySavedUsers
	var members string
	if err := rows.Scan(&row.OwnerID, &row.Kind, &members); err != nil {
		return row, err
	}
	if row.Kind != subscription.Friends && row.Kind != subscription.Blacklist {
		return row, errors.New("invalid contact list kind")
	}
	var err error
	row.Members, err = parseMemberIDs(members)
	return row, err
}

func parseMemberIDs(value string) ([]int64, error) {
	if strings.TrimSpace(value) == "" {
		return []int64{}, nil
	}
	var raw []json.RawMessage
	if err := json.Unmarshal([]byte(value), &raw); err != nil {
		return nil, errors.New("invalid contact list JSON")
	}
	names := make([]string, len(raw))
	for i, id := range raw {
		names[i] = string(id)
		if len(id) > 0 && id[0] == '"' {
			if err := json.Unmarshal(id, &names[i]); err != nil {
				return nil, errors.New("invalid contact member ID")
			}
		}
	}
	return uniqueIDs(names)
}

func uniqueIDs(values []string) ([]int64, error) {
	result := make([]int64, 0, len(values))
	seen := make(map[int64]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		id, err := strconv.ParseInt(value, 10, 64)
		if err != nil || id < 1 {
			return nil, errors.New("invalid source ID")
		}
		if _, known := seen[id]; !known {
			seen[id] = struct{}{}
			result = append(result, id)
		}
	}
	return result, nil
}
