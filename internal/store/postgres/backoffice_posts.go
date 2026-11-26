package postgres

import (
	"context"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/admin"
)

const reviewPostColumns = `p.id,p.owner_id,u.account,p.visibility,p.like_count,p.comment_count,p.price,p.status,p.reviewed_at,p.created_at`

func (s *Store) BackofficePostPage(ctx context.Context, q admin.PostReviewQuery) ([]admin.PostReviewRow, int64, error) {
	const relation = `FROM anchor_posts p JOIN users u ON u.id=p.owner_id WHERE NOT p.deleted
		AND ($1::bigint IS NULL OR p.owner_id=$1)
		AND ($2::text IS NULL OR starts_with(lower(u.account),lower($2)))
		AND ($3::smallint IS NULL OR p.status=$3)
		AND ($4::timestamptz IS NULL OR p.created_at >= $4) AND ($5::timestamptz IS NULL OR p.created_at <= $5)
		AND ($6::timestamptz IS NULL OR p.reviewed_at >= $6) AND ($7::timestamptz IS NULL OR p.reviewed_at <= $7)`
	return readBackofficePage(ctx, s, q.PageRequest, reviewPostColumns, relation, "p.created_at DESC,p.id DESC",
		[]any{q.AuthorID, q.AuthorName, q.Status, q.CreatedStart, q.CreatedEnd, q.ReviewedStart, q.ReviewedEnd}, func(row pgx.CollectableRow) (admin.PostReviewRow, error) {
			var p admin.PostReviewRow
			err := row.Scan(&p.ID, &p.Author, &p.AuthorName, &p.Visibility, &p.Likes, &p.Comments, &p.Price, &p.Status, &p.ReviewedAt, &p.CreatedAt)
			return p, err
		})
}

func (s *Store) BackofficePostDetail(ctx context.Context, id, author int64) (admin.PostReviewDetail, error) {
	var p admin.PostReviewDetail
	var generic int8
	p.Type = &generic
	err := s.pool.QueryRow(ctx, `SELECT `+reviewPostColumns+`,p.content,p.media::text FROM anchor_posts p
		JOIN users u ON u.id=p.owner_id WHERE p.id=$1 AND ($2::bigint=0 OR p.owner_id=$2)`, id, author).
		Scan(&p.ID, &p.Author, &p.AuthorName, &p.Visibility, &p.Likes, &p.Comments, &p.Price, &p.Status, &p.ReviewedAt, &p.CreatedAt, &p.Content, &p.Media)
	if errors.Is(err, pgx.ErrNoRows) {
		err = admin.ErrBackofficeNotFound
	}
	return p, err
}

func (s *Store) BackofficePostComments(ctx context.Context, id int64, p admin.PageRequest) ([]admin.PostCommentReview, int64, error) {
	return readBackofficePage(ctx, s, p, "c.id,c.author_id,c.content,c.created_at", `FROM anchor_post_comments c
		WHERE NOT c.deleted AND ($1::bigint=0 OR c.post_id=$1)`, "c.created_at DESC,c.id DESC", []any{id},
		func(row pgx.CollectableRow) (admin.PostCommentReview, error) {
			var item admin.PostCommentReview
			err := row.Scan(&item.ID, &item.Author, &item.Content, &item.CreatedAt)
			return item, err
		})
}

func (s *Store) RemoveBackofficePost(ctx context.Context, operator admin.Principal, path string, id, author int64) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var deleted bool
		err := tx.QueryRow(ctx, `SELECT deleted FROM anchor_posts WHERE id=$1 AND ($2::bigint=0 OR owner_id=$2) FOR UPDATE`,
			id, author).Scan(&deleted)
		if errors.Is(err, pgx.ErrNoRows) {
			return admin.ErrBackofficeNotFound
		}
		if err != nil || deleted {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE anchor_posts SET deleted=TRUE WHERE id=$1`, id); err != nil {
			return err
		}
		return auditBackofficePost(ctx, tx, operator, path, "post_delete", id)
	})
}

func (s *Store) RemoveBackofficePostComment(ctx context.Context, operator admin.Principal, path string, id, postID int64) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var post int64
		err := tx.QueryRow(ctx, `SELECT id FROM anchor_posts WHERE id=$1 FOR UPDATE`, postID).Scan(&post)
		if errors.Is(err, pgx.ErrNoRows) {
			return admin.ErrBackofficeNotFound
		}
		if err != nil {
			return err
		}
		var deleted bool
		err = tx.QueryRow(ctx, `SELECT deleted FROM anchor_post_comments WHERE id=$1 AND post_id=$2`, id, postID).Scan(&deleted)
		if errors.Is(err, pgx.ErrNoRows) {
			return admin.ErrBackofficeNotFound
		}
		if err != nil || deleted {
			return err
		}
		if err := softDeletePostCommentBranch(ctx, tx, id, postID); err != nil {
			return err
		}
		return auditBackofficePost(ctx, tx, operator, path, "post_comment_delete", id)
	})
}

func auditBackofficePost(ctx context.Context, tx pgx.Tx, operator admin.Principal, path, action string, id int64) error {
	_, err := tx.Exec(ctx, `INSERT INTO backoffice_logs(area,tenant,operator_id,operator_name,path,target,action)
		VALUES($1,$2,$3,$4,$5,$6,$7)`, operator.Area, operator.Scope(), operator.ID, operator.Name(), path,
		strconv.FormatInt(id, 10), action)
	return err
}
