package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"open_live_platform/internal/posts"
)

type postQuery interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func postAccess(ctx context.Context, q postQuery, actor, id int64, lock bool) (owner int64, full bool, status int, err error) {
	query := `SELECT p.owner_id,` + anchorPostFull + `,p.status FROM anchor_posts p WHERE p.id=$2 AND ` + visibleAnchorPost
	if lock {
		query += ` FOR UPDATE OF p`
	}
	err = q.QueryRow(ctx, query, actor, id).Scan(&owner, &full, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		err = posts.ErrNotFound
	}
	return
}

func canInteractWithPost(ctx context.Context, q postQuery, actor, id int64, lock bool) error {
	_, full, status, err := postAccess(ctx, q, actor, id, lock)
	if err != nil {
		return err
	}
	if !full || status != posts.Approved {
		return posts.ErrForbidden
	}
	return nil
}

func canWritePostMessage(ctx context.Context, tx pgx.Tx, actor int64) error {
	var blocked bool
	err := tx.QueryRow(ctx, `SELECT account_status IN (1,2) AND `+activeAccountLimit+` FROM users u WHERE u.id=$1 FOR SHARE`, actor).Scan(&blocked)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && blocked) {
		return posts.ErrForbidden
	}
	return err
}

func (s *Store) CreatePostComment(ctx context.Context, comment posts.Comment) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := canInteractWithPost(ctx, tx, comment.AuthorID, comment.PostID, true); err != nil {
			return err
		}
		if err := canWritePostMessage(ctx, tx, comment.AuthorID); err != nil {
			return err
		}
		if comment.ParentID != nil {
			var valid bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM anchor_post_comments WHERE id=$1 AND post_id=$2 AND NOT deleted)`,
				comment.ParentID, comment.PostID).Scan(&valid); err != nil {
				return err
			}
			if !valid {
				return posts.ErrNotFound
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO anchor_post_comments(post_id,author_id,content,parent_id) VALUES($1,$2,$3,$4)`,
			comment.PostID, comment.AuthorID, comment.Content, comment.ParentID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE anchor_posts SET comment_count=comment_count+1 WHERE id=$1`, comment.PostID)
		return err
	})
}

func (s *Store) PostComments(ctx context.Context, actor, id int64) ([]posts.Comment, error) {
	var result []posts.Comment
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if err := canInteractWithPost(ctx, tx, actor, id, false); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT c.id,c.post_id,c.author_id,c.parent_id,c.content,c.created_at,c.like_count,
			EXISTS(SELECT 1 FROM anchor_post_comment_likes l WHERE l.comment_id=c.id AND l.actor_id=$1)
			FROM anchor_post_comments c WHERE c.post_id=$2 AND NOT c.deleted ORDER BY c.created_at DESC,c.id DESC`, actor, id)
		if err != nil {
			return err
		}
		result, err = pgx.AppendRows([]posts.Comment{}, rows, func(row pgx.CollectableRow) (posts.Comment, error) {
			var c posts.Comment
			err := row.Scan(&c.ID, &c.PostID, &c.AuthorID, &c.ParentID, &c.Content, &c.CreatedAt, &c.Likes, &c.Liked)
			c.CreatedAt = c.CreatedAt.UTC()
			return c, err
		})
		return err
	})
	return result, err
}

func commentPostID(ctx context.Context, tx pgx.Tx, id int64) (int64, error) {
	var postID int64
	err := tx.QueryRow(ctx, `SELECT post_id FROM anchor_post_comments WHERE id=$1 AND NOT deleted`, id).Scan(&postID)
	if errors.Is(err, pgx.ErrNoRows) {
		err = posts.ErrNotFound
	}
	return postID, err
}

func (s *Store) DeletePostComment(ctx context.Context, actor, id int64) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		postID, err := commentPostID(ctx, tx, id)
		if err != nil {
			return err
		}
		owner, _, _, err := postAccess(ctx, tx, actor, postID, true)
		if err != nil {
			return err
		}
		var author int64
		err = tx.QueryRow(ctx, `SELECT author_id FROM anchor_post_comments WHERE id=$1 AND NOT deleted`, id).Scan(&author)
		if errors.Is(err, pgx.ErrNoRows) {
			return posts.ErrNotFound
		}
		if err != nil {
			return err
		}
		if author != actor && owner != actor {
			return posts.ErrForbidden
		}
		tag, err := tx.Exec(ctx, `WITH RECURSIVE branch AS (
			SELECT id FROM anchor_post_comments WHERE id=$1 AND NOT deleted
			UNION ALL SELECT c.id FROM anchor_post_comments c JOIN branch b ON c.parent_id=b.id WHERE c.post_id=$2 AND NOT c.deleted
		) UPDATE anchor_post_comments SET deleted=TRUE WHERE id IN (SELECT id FROM branch)`, id, postID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE anchor_posts SET comment_count=comment_count-$2 WHERE id=$1`, postID, tag.RowsAffected())
		return err
	})
}

func (s *Store) LikePost(ctx context.Context, actor, id int64, liked bool) (int64, error) {
	var count int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := canInteractWithPost(ctx, tx, actor, id, true); err != nil {
			return err
		}
		var tag pgconn.CommandTag
		var err error
		if liked {
			tag, err = tx.Exec(ctx, `INSERT INTO anchor_post_likes(post_id,actor_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, id, actor)
		} else {
			tag, err = tx.Exec(ctx, `DELETE FROM anchor_post_likes WHERE post_id=$1 AND actor_id=$2`, id, actor)
		}
		if err != nil {
			return err
		}
		count, err = updatePostLikeCount(ctx, tx, `anchor_posts`, id, tag.RowsAffected(), liked)
		return err
	})
	return count, err
}

func (s *Store) LikePostComment(ctx context.Context, actor, id int64, liked bool) (int64, error) {
	var count int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		postID, err := commentPostID(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := canInteractWithPost(ctx, tx, actor, postID, true); err != nil {
			return err
		}
		if _, err := commentPostID(ctx, tx, id); err != nil {
			return err
		}
		var tag pgconn.CommandTag
		if liked {
			tag, err = tx.Exec(ctx, `INSERT INTO anchor_post_comment_likes(comment_id,actor_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, id, actor)
		} else {
			tag, err = tx.Exec(ctx, `DELETE FROM anchor_post_comment_likes WHERE comment_id=$1 AND actor_id=$2`, id, actor)
		}
		if err != nil {
			return err
		}
		count, err = updatePostLikeCount(ctx, tx, `anchor_post_comments`, id, tag.RowsAffected(), liked)
		return err
	})
	return count, err
}

func updatePostLikeCount(ctx context.Context, tx pgx.Tx, table string, id, delta int64, like bool) (int64, error) {
	var count int64
	if delta == 0 {
		err := tx.QueryRow(ctx, `SELECT like_count FROM `+table+` WHERE id=$1`, id).Scan(&count)
		return count, err
	}
	if !like {
		delta = -delta
	}
	err := tx.QueryRow(ctx, `UPDATE `+table+` SET like_count=like_count+$2 WHERE id=$1 RETURNING like_count`, id, delta).Scan(&count)
	return count, err
}

func (s *Store) PostReactions(ctx context.Context, actor, id int64, tips bool, offset, limit int) (posts.Page[posts.Reaction], error) {
	var result posts.Page[posts.Reaction]
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if err := canInteractWithPost(ctx, tx, actor, id, false); err != nil {
			return err
		}
		source := `SELECT actor_id,0::bigint AS points,created_at FROM anchor_post_likes WHERE post_id=$1`
		order := `created_at DESC,actor_id DESC`
		if tips {
			source = `SELECT actor_id,sum(points)::bigint AS points,max(paid_at) AS created_at FROM anchor_post_orders
				WHERE post_id=$1 AND kind=2 AND state=1 GROUP BY actor_id`
			order = `points DESC,created_at DESC,actor_id DESC`
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM (`+source+`) reactions`, id).Scan(&result.Total); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT * FROM (`+source+`) reactions ORDER BY `+order+` LIMIT $2 OFFSET $3`, id, limit, offset)
		if err != nil {
			return err
		}
		result.Rows, err = pgx.AppendRows([]posts.Reaction{}, rows, func(row pgx.CollectableRow) (posts.Reaction, error) {
			var v posts.Reaction
			err := row.Scan(&v.UserID, &v.Points, &v.CreatedAt)
			v.CreatedAt = v.CreatedAt.UTC()
			return v, err
		})
		return err
	})
	return result, err
}
