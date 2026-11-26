package postgres

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"open_live_platform/internal/posts"
)

type LegacyPost struct {
	posts.Post
	Buyers     []int64
	ReviewedAt *time.Time
}

type LegacyPostReaction struct {
	TargetID, ActorID, Points int64
	CreatedAt                 time.Time
}

type LegacyShare struct {
	ID, OwnerID              int64
	Content, Photo, Comments string
	Likes, Status            int
	CreatedAt, UpdatedAt     time.Time
}

type LegacyGroup struct {
	ID, OwnerID int64
	Name        string
	Kind        int
	Deleted     bool
	Members     []int64
}

type LegacySavedUsers struct {
	OwnerID int64
	Kind    int
	Members []int64
}

type LegacyPublishing struct {
	Posts                         iter.Seq2[LegacyPost, error]
	Comments                      iter.Seq2[posts.Comment, error]
	PostLikes, CommentLikes, Tips iter.Seq2[LegacyPostReaction, error]
	Shares                        iter.Seq2[LegacyShare, error]
	Groups                        iter.Seq2[LegacyGroup, error]
	Saved                         iter.Seq2[LegacySavedUsers, error]
}

type PublishingImportCounts struct {
	Posts, Comments, PostLikes, CommentLikes, Tips, Unlocks, Shares, Groups, Saved, Existing, Skipped int64
}

func (s *Store) ImportLegacyPublishing(ctx context.Context, data LegacyPublishing) (PublishingImportCounts, error) {
	var counts PublishingImportCounts
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `LOCK TABLE anchor_posts,anchor_post_comments,anchor_post_likes,anchor_post_comment_likes,
			legacy_anchor_post_unlocks,legacy_anchor_post_tips,public_shares,subscription_groups,subscription_group_members,
			subscription_saved_users,legacy_subscription_lists_imported IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			return err
		}
		for row, err := range data.Posts {
			if err != nil {
				return err
			}
			if err := importLegacyPost(ctx, tx, row, &counts); err != nil {
				return fmt.Errorf("post %d: %w", row.ID, err)
			}
		}
		if err := importLegacyPostComments(ctx, tx, data.Comments, &counts); err != nil {
			return err
		}
		for _, set := range []struct {
			source iter.Seq2[LegacyPostReaction, error]
			target string
			count  *int64
		}{{data.PostLikes, "post_like", &counts.PostLikes}, {data.CommentLikes, "comment_like", &counts.CommentLikes}, {data.Tips, "tip", &counts.Tips}} {
			for row, err := range set.source {
				if err != nil {
					return err
				}
				if err := importLegacyPostReaction(ctx, tx, set.target, row, set.count, &counts); err != nil {
					return fmt.Errorf("%s for %d: %w", set.target, row.TargetID, err)
				}
			}
		}
		for row, err := range data.Shares {
			if err != nil {
				return err
			}
			if err := importLegacyShare(ctx, tx, row, &counts); err != nil {
				return fmt.Errorf("share %d: %w", row.ID, err)
			}
		}
		for row, err := range data.Groups {
			if err != nil {
				return err
			}
			if err := importLegacyGroup(ctx, tx, row, &counts); err != nil {
				return fmt.Errorf("group %d: %w", row.ID, err)
			}
		}
		for row, err := range data.Saved {
			if err != nil {
				return err
			}
			if err := importLegacySavedUsers(ctx, tx, row, &counts); err != nil {
				return fmt.Errorf("contact list for %d: %w", row.OwnerID, err)
			}
		}
		return finishLegacyPublishing(ctx, tx)
	})
	if err != nil {
		return PublishingImportCounts{}, err
	}
	return counts, nil
}

func importLegacyPost(ctx context.Context, tx pgx.Tx, row LegacyPost, counts *PublishingImportCounts) error {
	if row.ID < 1 || row.OwnerID < 1 || row.CreatedAt.IsZero() || row.Status < posts.Pending || row.Status > posts.Approved {
		return errors.New("invalid identity, timestamp or review status")
	}
	tag, err := tx.Exec(ctx, `INSERT INTO anchor_posts(id,owner_id,content,visibility,price,media,only_text,images,videos,
		video_length,reference_post,status,deleted,created_at,reviewed_at,tip_points,livemedia_imported)
		SELECT $1,u.id,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,true
		FROM users u WHERE u.id=$2 ON CONFLICT (id) DO NOTHING`, row.ID, row.OwnerID, row.Content, row.Level, row.Price,
		row.Media, row.OnlyText, row.Images, row.Videos, row.VideoLength, row.Reference, row.Status, row.Deleted, row.CreatedAt, row.ReviewedAt, row.TipPoints)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return legacyPublishingExisting(ctx, tx, "anchor_posts", row.ID, row.OwnerID, "owner_id", counts)
	}
	counts.Posts++
	if row.Level != 0 || row.Price == 0 || len(row.Buyers) == 0 {
		return nil
	}
	tag, err = tx.Exec(ctx, `INSERT INTO legacy_anchor_post_unlocks(post_id,actor_id)
		SELECT $1,id FROM users WHERE id=ANY($2) AND id<>$3 ON CONFLICT DO NOTHING`, row.ID, row.Buyers, row.OwnerID)
	counts.Unlocks += tag.RowsAffected()
	counts.Skipped += int64(len(row.Buyers)) - tag.RowsAffected()
	return err
}

func importLegacyPostComments(ctx context.Context, tx pgx.Tx, source iter.Seq2[posts.Comment, error], counts *PublishingImportCounts) error {
	_, err := tx.Exec(ctx, `CREATE TEMP TABLE source_post_comments(
		id bigint PRIMARY KEY,post_id bigint,author_id bigint,parent_id bigint,content text,deleted boolean,created_at timestamptz
	) ON COMMIT DROP`)
	if err != nil {
		return err
	}
	next, stop := iter.Pull2(source)
	defer stop()
	total, err := tx.CopyFrom(ctx, pgx.Identifier{"source_post_comments"}, []string{"id", "post_id", "author_id", "parent_id", "content", "deleted", "created_at"},
		pgx.CopyFromFunc(func() ([]any, error) {
			row, err, ok := next()
			if err != nil || !ok {
				return nil, err
			}
			if row.ID < 1 || row.PostID < 1 || row.AuthorID < 1 || row.CreatedAt.IsZero() {
				return nil, errors.New("invalid source comment identity or timestamp")
			}
			return []any{row.ID, row.PostID, row.AuthorID, row.ParentID, row.Content, row.Deleted, row.CreatedAt}, nil
		}))
	if err != nil {
		return err
	}
	var collision bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM source_post_comments st JOIN anchor_post_comments c ON c.id=st.id
		WHERE NOT c.livemedia_imported OR c.author_id<>st.author_id OR c.post_id<>st.post_id)`).Scan(&collision); err != nil {
		return err
	}
	if collision {
		return errors.New("source comment ID collides with another target comment")
	}
	var existing int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM source_post_comments st JOIN anchor_post_comments c ON c.id=st.id`).Scan(&existing); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `WITH RECURSIVE eligible AS (
		SELECT st.* FROM source_post_comments st JOIN anchor_posts p ON p.id=st.post_id AND p.livemedia_imported
		JOIN users u ON u.id=st.author_id
	), tree AS (
		SELECT * FROM eligible WHERE parent_id IS NULL
		UNION ALL
		SELECT c.id,c.post_id,c.author_id,c.parent_id,c.content,(c.deleted OR parent.deleted),c.created_at
		FROM eligible c JOIN tree parent ON c.parent_id=parent.id AND c.post_id=parent.post_id
	)
	INSERT INTO anchor_post_comments(id,post_id,author_id,parent_id,content,deleted,created_at,livemedia_imported)
	SELECT id,post_id,author_id,parent_id,content,deleted,created_at,true FROM tree ON CONFLICT (id) DO NOTHING`)
	counts.Comments += tag.RowsAffected()
	counts.Existing += existing
	counts.Skipped += total - existing - tag.RowsAffected()
	return err
}

func importLegacyPostReaction(ctx context.Context, tx pgx.Tx, kind string, row LegacyPostReaction, n *int64, counts *PublishingImportCounts) error {
	var query string
	switch kind {
	case "post_like":
		query = `INSERT INTO anchor_post_likes(post_id,actor_id,created_at)
			SELECT p.id,u.id,$3 FROM anchor_posts p JOIN users u ON u.id=$2 WHERE p.id=$1 AND p.livemedia_imported
			ON CONFLICT DO NOTHING`
	case "comment_like":
		query = `INSERT INTO anchor_post_comment_likes(comment_id,actor_id,created_at)
			SELECT c.id,u.id,$3 FROM anchor_post_comments c JOIN users u ON u.id=$2 WHERE c.id=$1 AND c.livemedia_imported AND NOT c.deleted
			ON CONFLICT DO NOTHING`
	case "tip":
		query = `INSERT INTO legacy_anchor_post_tips(post_id,actor_id,tipped_at,points)
			SELECT p.id,u.id,$3,$4 FROM anchor_posts p JOIN users u ON u.id=$2 WHERE p.id=$1 AND p.livemedia_imported
			ON CONFLICT DO NOTHING`
	}
	args := []any{row.TargetID, row.ActorID, row.CreatedAt}
	if kind == "tip" {
		args = append(args, row.Points)
	}
	if row.ActorID < 1 || row.TargetID < 1 || row.CreatedAt.IsZero() || (kind == "tip" && row.Points < 1) {
		counts.Skipped++
		return nil
	}
	tag, err := tx.Exec(ctx, query, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		*n++
		return nil
	}
	var exists bool
	target, object := "anchor_post_likes", "post_id"
	if kind == "comment_like" {
		target, object = "anchor_post_comment_likes", "comment_id"
	}
	if kind == "tip" {
		target = "legacy_anchor_post_tips"
	}
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+target+` WHERE `+object+`=$1 AND actor_id=$2)`, row.TargetID, row.ActorID).Scan(&exists)
	if err == nil {
		if exists {
			counts.Existing++
		} else {
			counts.Skipped++
		}
	}
	return err
}

func importLegacyShare(ctx context.Context, tx pgx.Tx, row LegacyShare, counts *PublishingImportCounts) error {
	if row.ID < 1 || row.OwnerID < 1 || row.CreatedAt.IsZero() || row.UpdatedAt.IsZero() || row.Likes < 0 {
		return errors.New("invalid identity, timestamp or count")
	}
	tag, err := tx.Exec(ctx, `INSERT INTO public_shares(id,user_id,content,photo,comments,likes,status,created_at,updated_at,livemedia_imported)
		SELECT $1,u.id,$3,$4,$5,$6,$7,$8,$9,true FROM users u WHERE u.id=$2 ON CONFLICT (id) DO NOTHING`,
		row.ID, row.OwnerID, row.Content, row.Photo, row.Comments, row.Likes, row.Status, row.CreatedAt, row.UpdatedAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return legacyPublishingExisting(ctx, tx, "public_shares", row.ID, row.OwnerID, "user_id", counts)
	}
	counts.Shares++
	return nil
}

func legacyPublishingExisting(ctx context.Context, tx pgx.Tx, table string, id, owner int64, ownerColumn string, counts *PublishingImportCounts) error {
	var same bool
	err := tx.QueryRow(ctx, `SELECT livemedia_imported AND `+ownerColumn+`=$2 FROM `+table+` WHERE id=$1`, id, owner).Scan(&same)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		counts.Skipped++
	case err != nil:
		return err
	case !same:
		return errors.New("source ID collides with another target record")
	default:
		counts.Existing++
	}
	return nil
}

func importLegacyGroup(ctx context.Context, tx pgx.Tx, row LegacyGroup, counts *PublishingImportCounts) error {
	if row.Deleted || row.ID < 1 || row.OwnerID < 1 || row.Kind < 0 || row.Kind > 3 {
		counts.Skipped++
		return nil
	}
	tag, err := tx.Exec(ctx, `INSERT INTO subscription_groups(id,owner_id,kind,name,livemedia_imported)
		SELECT $1,u.id,$3,$4,true FROM users u WHERE u.id=$2 ON CONFLICT DO NOTHING`, row.ID, row.OwnerID, row.Kind, row.Name)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return legacyPublishingExisting(ctx, tx, "subscription_groups", row.ID, row.OwnerID, "owner_id", counts)
	}
	counts.Groups++
	if len(row.Members) == 0 {
		return nil
	}
	tag, err = tx.Exec(ctx, `INSERT INTO subscription_group_members(group_id,person_id)
		SELECT $1,u.id FROM unnest($2::bigint[]) WITH ORDINALITY source(id,position)
		JOIN users u ON u.id=source.id WHERE u.id<>$3 ORDER BY source.position ON CONFLICT DO NOTHING`, row.ID, row.Members, row.OwnerID)
	counts.Skipped += int64(len(row.Members)) - tag.RowsAffected()
	return err
}

func importLegacySavedUsers(ctx context.Context, tx pgx.Tx, row LegacySavedUsers, counts *PublishingImportCounts) error {
	tag, err := tx.Exec(ctx, `INSERT INTO legacy_subscription_lists_imported(owner_id,kind)
		SELECT id,$2 FROM users WHERE id=$1 ON CONFLICT DO NOTHING`, row.OwnerID, row.Kind)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		var known bool
		err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM legacy_subscription_lists_imported WHERE owner_id=$1 AND kind=$2)`,
			row.OwnerID, row.Kind).Scan(&known)
		if known {
			counts.Existing++
		} else {
			counts.Skipped++
		}
		return err
	}
	counts.Saved++
	if len(row.Members) == 0 {
		return nil
	}
	var saved pgconn.CommandTag
	saved, err = tx.Exec(ctx, `INSERT INTO subscription_saved_users(owner_id,person_id,kind)
		SELECT $1,id,$3 FROM users WHERE id=ANY($2) AND id<>$1 ON CONFLICT DO NOTHING`, row.OwnerID, row.Members, row.Kind)
	counts.Skipped += int64(len(row.Members)) - saved.RowsAffected()
	return err
}

func finishLegacyPublishing(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `UPDATE anchor_posts p SET
		like_count=(SELECT count(*) FROM anchor_post_likes l WHERE l.post_id=p.id),
		comment_count=(SELECT count(*) FROM anchor_post_comments c WHERE c.post_id=p.id AND NOT c.deleted)
		WHERE p.livemedia_imported;
		UPDATE anchor_post_comments c SET like_count=(SELECT count(*) FROM anchor_post_comment_likes l WHERE l.comment_id=c.id)
		WHERE c.livemedia_imported;
		SELECT setval('anchor_posts_id_seq', max(id), true) FROM anchor_posts HAVING max(id)>=(SELECT last_value FROM anchor_posts_id_seq);
		SELECT setval('anchor_post_comments_id_seq', max(id), true) FROM anchor_post_comments HAVING max(id)>=(SELECT last_value FROM anchor_post_comments_id_seq);
		SELECT setval('public_shares_id_seq', max(id), true) FROM public_shares HAVING max(id)>=(SELECT last_value FROM public_shares_id_seq);
		SELECT setval('subscription_groups_id_seq', max(id), true) FROM subscription_groups HAVING max(id)>=(SELECT last_value FROM subscription_groups_id_seq)`)
	return err
}
