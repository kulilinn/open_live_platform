package postgres

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"open_live_platform/internal/collectible"
)

var _ collectible.ManagedNFTStore = (*Store)(nil)

func (s *Store) ManagedNFTCards(ctx context.Context) ([]collectible.ManagedCard, error) {
	rows, err := s.pool.Query(ctx, `SELECT coalesce(c.id,b.token_id), NULLIF(c.name,''), coalesce(c.english,''),coalesce(c.chinese,''),
		coalesce(c.picture,''),coalesce(c.status,0),coalesce(c.order_account,NULLIF(c.address,'')),c.pre_order_account,c.pre_order_name,c.description,
		c.created_at,c.updated_at,c.sale_at,c.sold_at,b.user_id,b.address,c.id IS NOT NULL
		FROM nft_card_inventory c FULL OUTER JOIN nft_bindings b ON b.token_id=c.id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (collectible.ManagedCard, error) {
		var c collectible.ManagedCard
		err := row.Scan(&c.ID, &c.Name, &c.English, &c.Chinese, &c.Picture, &c.Status, &c.OrderAccount, &c.PreOrderAccount,
			&c.PreOrderName, &c.Description, &c.Created, &c.Updated, &c.Sale, &c.Sold, &c.UserID, &c.Account, &c.Inventory)
		for _, timestamp := range []*time.Time{c.Created, c.Updated, c.Sale, c.Sold} {
			if timestamp != nil {
				*timestamp = timestamp.UTC()
			}
		}
		return c, err
	})
}

type nftRequestReader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func (s *Store) HasManagedNFTRequest(ctx context.Context, req collectible.ManagedNFTRequest) (bool, error) {
	return hasManagedNFTRequest(ctx, s.pool, req)
}

func hasManagedNFTRequest(ctx context.Context, db nftRequestReader, req collectible.ManagedNFTRequest) (bool, error) {
	var digest []byte
	err := db.QueryRow(ctx, `SELECT fingerprint FROM nft_backoffice_requests WHERE actor_id=$1 AND request_key=$2`, req.Actor, req.Key).Scan(&digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !slices.Equal(digest, req.Digest[:]) {
		return false, collectible.ErrManagedNFTConflict
	}
	return true, nil
}

func (s *Store) nftManagementChange(ctx context.Context, req collectible.ManagedNFTRequest, edit func(pgx.Tx) error) error {
	return s.nftChange(ctx, func(tx pgx.Tx) error {
		if req.Key != "" {
			tag, err := tx.Exec(ctx, `INSERT INTO nft_backoffice_requests(actor_id,request_key,fingerprint) VALUES($1,$2,$3)
				ON CONFLICT DO NOTHING`, req.Actor, req.Key, req.Digest[:])
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				_, err := hasManagedNFTRequest(ctx, tx, req)
				return err
			}
		}
		return edit(tx)
	})
}

func (s *Store) EditManagedNFTCard(ctx context.Context, req collectible.ManagedNFTRequest, edit collectible.ManagedCardEdit, seeds []collectible.Card) error {
	return s.nftManagementChange(ctx, req, func(tx pgx.Tx) error {
		if err := seedManagedNFTCards(ctx, tx, seeds); err != nil {
			return err
		}
		var exists bool
		err := tx.QueryRow(ctx, `UPDATE nft_card_inventory SET status=$2,name=coalesce($3,''),pre_order_account=$4,pre_order_name=$5,
			description=$6,sale_at=$7,updated_at=CASE WHEN (status,name,pre_order_account,pre_order_name,description,sale_at)
			IS DISTINCT FROM ($2::smallint,coalesce($3::text,''),$4::text,$5::text,$6::text,$7::timestamptz)
			THEN statement_timestamp() ELSE updated_at END WHERE id=$1 RETURNING true`,
			edit.ID, edit.Status, edit.Name, edit.PreOrderAccount, edit.PreOrderName, edit.Description, edit.Sale).Scan(&exists)
		if errors.Is(err, pgx.ErrNoRows) {
			return collectible.ErrNotFound
		}
		return err
	})
}

func (s *Store) SetManagedNFTSale(ctx context.Context, req collectible.ManagedNFTRequest, ids []uint32, sale time.Time, seeds []collectible.Card) error {
	dbIDs := make([]int64, len(ids))
	for i, id := range ids {
		dbIDs[i] = int64(id)
	}
	return s.nftManagementChange(ctx, req, func(tx pgx.Tx) error {
		if err := seedManagedNFTCards(ctx, tx, seeds); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE nft_card_inventory SET sale_at=$2,
			updated_at=CASE WHEN sale_at IS DISTINCT FROM $2 THEN statement_timestamp() ELSE updated_at END
			WHERE id=ANY($1::bigint[])`, dbIDs, sale)
		if err == nil && tag.RowsAffected() != int64(len(dbIDs)) {
			return collectible.ErrNotFound
		}
		return err
	})
}

func seedManagedNFTCards(ctx context.Context, tx pgx.Tx, cards []collectible.Card) error {
	for _, card := range cards {
		_, err := tx.Exec(ctx, `INSERT INTO nft_card_inventory(id,name,english,chinese,picture,address,status) VALUES($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT DO NOTHING`, card.ID, card.Name, card.English, card.Chinese, card.Picture, card.Address, card.Status)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) BindManagedNFT(ctx context.Context, req collectible.ManagedNFTRequest, userID int64, b collectible.Binding, slots map[int8]int32) error {
	return s.nftManagementChange(ctx, req, func(tx pgx.Tx) error {
		if err := managedNFTUserExists(ctx, tx, userID); err != nil {
			return err
		}
		return bindNFTAccount(ctx, tx, userID, b, slots, true)
	})
}

func (s *Store) UnbindManagedNFT(ctx context.Context, req collectible.ManagedNFTRequest, userID int64) error {
	return s.nftManagementChange(ctx, req, func(tx pgx.Tx) error {
		if err := managedNFTUserExists(ctx, tx, userID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `WITH removed AS (DELETE FROM nft_bindings WHERE user_id=$1 RETURNING *)
			INSERT INTO nft_binding_events(user_id,token_id,address,bound) SELECT user_id,token_id,address,FALSE FROM removed`, userID)
		return err
	})
}

func managedNFTUserExists(ctx context.Context, tx pgx.Tx, userID int64) error {
	var id int64
	err := tx.QueryRow(ctx, "SELECT id FROM users WHERE id=$1 FOR KEY SHARE", userID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return collectible.ErrNotFound
	}
	return err
}

func (s *Store) ImportLegacyNFTInventory(ctx context.Context, cards []collectible.ManagedCard) (int, error) {
	var imported int
	err := s.nftChange(ctx, func(tx pgx.Tx) error {
		for _, card := range cards {
			tag, err := tx.Exec(ctx, `INSERT INTO nft_card_inventory(id,name,english,chinese,picture,address,status,order_account,
				pre_order_account,pre_order_name,description,created_at,updated_at,sale_at,sold_at)
				VALUES($1,coalesce($2,''),$3,$4,$5,'',$6,$7,$8,$9,$10,$11,$12,$13,$14) ON CONFLICT DO NOTHING`,
				card.ID, card.Name, card.English, card.Chinese, card.Picture, card.Status, card.OrderAccount, card.PreOrderAccount,
				card.PreOrderName, card.Description, card.Created, card.Updated, card.Sale, card.Sold)
			if err != nil {
				return err
			}
			imported += int(tag.RowsAffected())
		}
		return nil
	})
	return imported, err
}
