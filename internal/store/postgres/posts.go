package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/posts"
)

var _ posts.Store = (*Store)(nil)

const anchorPostFields = `p.id,p.owner_id,p.content,p.visibility,p.price,p.media,p.only_text,p.images,p.videos,p.video_length,p.reference_post,
	p.status,p.deleted,p.created_at,p.like_count,p.comment_count,p.tip_points`
const anchorPostBought = `EXISTS(SELECT 1 FROM anchor_post_orders o WHERE o.post_id=p.id AND o.actor_id=$1 AND o.kind=1 AND o.state=1)`
const anchorPostFull = `(p.owner_id=$1 OR (p.visibility=0 AND (p.price=0 OR ` + anchorPostBought + `)) OR (p.visibility>=2 AND EXISTS (
	SELECT 1 FROM subscription_members m JOIN subscription_plans plan ON plan.id=m.plan_id
	WHERE m.anchor_id=p.owner_id AND m.fan_id=$1 AND m.cancelled_at IS NULL AND m.expires_at>now()
	AND COALESCE(m.legacy_level,plan.plan_type-2) BETWEEN (p.visibility-2) AND 2)))`
const visibleAnchorPost = `NOT p.deleted AND (p.owner_id=$1 OR (p.status=3 AND p.visibility<>1))`
const anchorPostViewerFields = anchorPostFields + `,` + anchorPostFull + `,` + anchorPostBought + `,
	EXISTS(SELECT 1 FROM anchor_post_likes l WHERE l.post_id=p.id AND l.actor_id=$1),
	COALESCE((SELECT sum(points) FROM anchor_post_orders o WHERE o.post_id=p.id AND o.actor_id=$1 AND o.kind=2 AND o.state=1),0)::bigint`

func scanAnchorPost(row pgx.Row) (posts.Post, error) {
	var p posts.Post
	err := row.Scan(&p.ID, &p.OwnerID, &p.Content, &p.Level, &p.Price, &p.Media, &p.OnlyText, &p.Images, &p.Videos, &p.VideoLength, &p.Reference,
		&p.Status, &p.Deleted, &p.CreatedAt, &p.LikeCount, &p.CommentCount, &p.TipPoints, &p.Full, &p.Bought, &p.Liked, &p.MyTip)
	p.CreatedAt = p.CreatedAt.UTC()
	if errors.Is(err, pgx.ErrNoRows) {
		err = posts.ErrNotFound
	}
	return p, err
}

func (s *Store) CreatePost(ctx context.Context, actor int64, input posts.Input, status int) (posts.Post, error) {
	return scanAnchorPost(s.pool.QueryRow(ctx, `INSERT INTO anchor_posts AS p(owner_id,content,visibility,price,media,only_text,images,videos,video_length,reference_post,status)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING `+anchorPostFields+`,TRUE,FALSE,FALSE,0`, actor, input.Content, input.Level, input.Price, input.Media,
		input.OnlyText, input.Images, input.Videos, input.VideoLength, input.Reference, status))
}

func (s *Store) ReviewPost(ctx context.Context, id int64, status int) error {
	tag, err := s.pool.Exec(ctx, `UPDATE anchor_posts SET status=$2,reviewed_at=COALESCE(reviewed_at,now()) WHERE id=$1 AND NOT deleted AND status IN (1,$2)`, id, status)
	if err != nil || tag.RowsAffected() != 0 {
		return err
	}
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM anchor_posts WHERE id=$1 AND NOT deleted)`, id).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return posts.ErrNotFound
	}
	return posts.ErrReviewConflict
}

func (s *Store) GetPost(ctx context.Context, actor, id int64) (posts.Post, error) {
	return scanAnchorPost(s.pool.QueryRow(ctx, `SELECT `+anchorPostViewerFields+` FROM anchor_posts p WHERE p.id=$2 AND `+visibleAnchorPost, actor, id))
}

const matchingAnchorPosts = visibleAnchorPost + ` AND ($2=0 OR p.owner_id=$2) AND ($2<>0 OR p.status=3) AND (p.visibility=1)=$3
	AND CASE $4::int WHEN 1 THEN p.images>0 WHEN 2 THEN p.videos>0 WHEN 3 THEN (p.images>0 OR p.videos>0) ELSE TRUE END
	AND CASE $5::int WHEN 1 THEN ` + anchorPostBought + ` WHEN 2 THEN p.visibility=3 WHEN 3 THEN p.price>0 WHEN 4 THEN p.visibility=2 WHEN 5 THEN p.visibility=4 ELSE TRUE END
	AND ($6='' OR ( (` + anchorPostFull + ` OR p.only_text=1) AND strpos(lower(p.content),lower($6))>0))`

func (s *Store) ListPosts(ctx context.Context, f posts.Filter) (posts.Page[posts.Post], error) {
	var result posts.Page[posts.Post]
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM anchor_posts p WHERE `+matchingAnchorPosts, f.Viewer, f.Owner, f.OwnOnly, f.Media, f.Kind, f.Search).Scan(&result.Total); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+anchorPostViewerFields+` FROM anchor_posts p WHERE `+matchingAnchorPosts+` AND ($7=0 OR p.id<$7)
			ORDER BY CASE WHEN $8=6 THEN p.created_at END DESC,CASE WHEN $8=7 THEN p.tip_points END DESC,
			CASE WHEN $8=8 THEN p.like_count END DESC,CASE WHEN $8=9 THEN p.created_at END ASC,p.id DESC LIMIT $9 OFFSET $10`,
			f.Viewer, f.Owner, f.OwnOnly, f.Media, f.Kind, f.Search, f.Before, f.Order, f.Size, f.Offset)
		if err != nil {
			return err
		}
		result.Rows, err = pgx.AppendRows([]posts.Post{}, rows, func(row pgx.CollectableRow) (posts.Post, error) { return scanAnchorPost(row) })
		return err
	})
	return result, err
}

func (s *Store) DeletePost(ctx context.Context, actor, id int64) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var owner int64
		var deleted bool
		err := tx.QueryRow(ctx, `SELECT owner_id,deleted FROM anchor_posts WHERE id=$1 FOR UPDATE`, id).Scan(&owner, &deleted)
		if errors.Is(err, pgx.ErrNoRows) {
			return posts.ErrNotFound
		}
		if err != nil {
			return err
		}
		if owner != actor {
			return posts.ErrForbidden
		}
		if deleted {
			return posts.ErrDeleted
		}
		_, err = tx.Exec(ctx, `UPDATE anchor_posts SET deleted=TRUE WHERE id=$1`, id)
		return err
	})
}

func (s *Store) PostStats(ctx context.Context, owner int64) (posts.Stats, error) {
	var result posts.Stats
	err := s.pool.QueryRow(ctx, `SELECT count(*),COALESCE(sum(images),0),COALESCE(sum(videos),0) FROM anchor_posts
		WHERE owner_id=$1 AND status=3 AND NOT deleted AND visibility<>1`, owner).Scan(&result.Posts, &result.Images, &result.Videos)
	return result, err
}

func (s *Store) RecentPostAnchors(ctx context.Context, limit int) ([]int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT owner_id FROM anchor_posts WHERE status=3 AND NOT deleted AND visibility<>1
		GROUP BY owner_id ORDER BY max(created_at) DESC,owner_id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

func (s *Store) PostBuyerCount(ctx context.Context, id int64) (int64, error) {
	var total int64
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM anchor_post_orders WHERE post_id=$1 AND kind=1 AND state=1`, id).Scan(&total)
	return total, err
}
