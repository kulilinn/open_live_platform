package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/collectible"
)

var _ collectible.Store = (*Store)(nil)

func (s *Store) SaveNFTChallenge(ctx context.Context, c collectible.Challenge) error {
	_, err := s.pool.Exec(ctx, `
		WITH expired AS (DELETE FROM nft_challenges WHERE expires_at <= statement_timestamp())
		INSERT INTO nft_challenges(id,user_id,message,expires_at) VALUES($1,$2,$3,$4)`,
		c.ID, c.UserID, c.Message, c.Expires)
	return err
}

func (s *Store) NFTChallenge(ctx context.Context, userID, id int64) (collectible.Challenge, error) {
	challenge := collectible.Challenge{UserID: userID, ID: id}
	err := s.pool.QueryRow(ctx, `SELECT message,expires_at FROM nft_challenges
		WHERE id=$1 AND user_id=$2 AND expires_at > statement_timestamp()`, id, userID).Scan(&challenge.Message, &challenge.Expires)
	if errors.Is(err, pgx.ErrNoRows) {
		err = collectible.ErrSignature
	}
	return challenge, err
}

func (s *Store) NFTState(ctx context.Context, userID int64) (collectible.State, error) {
	state := collectible.State{Slots: make(map[int8]int32)}
	rows, err := s.pool.Query(ctx, `SELECT b.token_id,b.address,a.slot,a.adorn_id FROM nft_bindings b
		LEFT JOIN nft_equipped_adorn a USING(user_id) WHERE b.user_id=$1`, userID)
	if err != nil {
		return state, err
	}
	defer rows.Close()
	for rows.Next() {
		var binding collectible.Binding
		var slot *int8
		var id *int32
		if err := rows.Scan(&binding.TokenID, &binding.Address, &slot, &id); err != nil {
			return state, err
		}
		state.Binding = &binding
		if slot != nil && id != nil {
			state.Slots[*slot] = *id
		}
	}
	return state, rows.Err()
}

func (s *Store) nftChange(ctx context.Context, edit func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// Moving a token can change two users' state. Order NFT mutations on one lock.
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(167884962, 34)"); err != nil {
			return err
		}
		return edit(tx)
	})
}

func (s *Store) BindNFT(ctx context.Context, c collectible.Challenge, b collectible.Binding, slots map[int8]int32) error {
	return s.nftChange(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM nft_challenges
			WHERE id=$1 AND user_id=$2 AND message=$3 AND expires_at > statement_timestamp()`, c.ID, c.UserID, c.Message)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return collectible.ErrSignature
		}
		return bindNFTAccount(ctx, tx, c.UserID, b, slots, false)
	})
}

func bindNFTAccount(ctx context.Context, tx pgx.Tx, userID int64, b collectible.Binding, slots map[int8]int32, idempotent bool) error {
	var previous collectible.Binding
	err := tx.QueryRow(ctx, `SELECT token_id,address FROM nft_bindings WHERE user_id=$1`, userID).
		Scan(&previous.TokenID, &previous.Address)
	if err == nil {
		if idempotent && previous == b {
			return nil
		}
		return collectible.ErrAlready
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if _, err := tx.Exec(ctx, `
			WITH removed AS (DELETE FROM nft_bindings WHERE token_id=$1 RETURNING *)
			INSERT INTO nft_binding_events(user_id,token_id,address,bound)
			SELECT user_id,token_id,address,FALSE FROM removed`, b.TokenID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO nft_bindings(user_id,token_id,address) VALUES($1,$2,$3)`,
		userID, b.TokenID, b.Address); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO nft_binding_events(user_id,token_id,address,bound) VALUES($1,$2,$3,TRUE)`,
		userID, b.TokenID, b.Address); err != nil {
		return err
	}
	return equipNFTSlots(ctx, tx, userID, slots)
}

func (s *Store) UnbindNFT(ctx context.Context, userID int64, expected *collectible.Binding) error {
	var token any
	var address string
	if expected != nil {
		token, address = expected.TokenID, expected.Address
	}
	return s.nftChange(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			WITH removed AS (DELETE FROM nft_bindings
			 WHERE user_id=$1 AND ($2::bigint IS NULL OR (token_id=$2 AND address=$3)) RETURNING *)
			INSERT INTO nft_binding_events(user_id,token_id,address,bound)
			SELECT user_id,token_id,address,FALSE FROM removed`, userID, token, address)
		if err == nil && expected == nil && tag.RowsAffected() == 0 {
			return collectible.ErrUnbound
		}
		return err
	})
}

func (s *Store) EquipNFT(ctx context.Context, userID int64, b collectible.Binding, slots map[int8]int32) error {
	return s.nftChange(ctx, func(tx pgx.Tx) error {
		var bound bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM nft_bindings
			WHERE user_id=$1 AND token_id=$2 AND address=$3)`, userID, b.TokenID, b.Address).Scan(&bound); err != nil {
			return err
		}
		if !bound {
			return collectible.ErrEquip
		}
		return equipNFTSlots(ctx, tx, userID, slots)
	})
}

func equipNFTSlots(ctx context.Context, tx pgx.Tx, userID int64, slots map[int8]int32) error {
	for slot, adornID := range slots {
		if _, err := tx.Exec(ctx, `INSERT INTO nft_equipped_adorn(user_id,slot,adorn_id) VALUES($1,$2,$3)
			ON CONFLICT(user_id,slot) DO UPDATE SET adorn_id=EXCLUDED.adorn_id`, userID, slot, adornID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) UnequipNFT(ctx context.Context, userID int64, slot int8, id int32) error {
	return s.nftChange(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM nft_equipped_adorn WHERE user_id=$1
			AND CASE WHEN $2::smallint=0 THEN adorn_id=$3 ELSE slot=$2 END`, userID, slot, id)
		return err
	})
}

// ImportLegacyNFT takes a binding already verified against the chain and does
// not restore it after any newer binding or unbinding in this database.
func (s *Store) ImportLegacyNFT(ctx context.Context, userID int64, b collectible.Binding, slots map[int8]int32) (bool, error) {
	var imported bool
	err := s.nftChange(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO nft_bindings(user_id,token_id,address)
			SELECT $1,$2,$3 WHERE EXISTS(SELECT 1 FROM users WHERE id=$1) AND NOT EXISTS(
				SELECT 1 FROM nft_binding_events WHERE user_id=$1 OR token_id=$2)
			ON CONFLICT DO NOTHING`, userID, b.TokenID, b.Address)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO nft_binding_events(user_id,token_id,address,bound) VALUES($1,$2,$3,TRUE)`,
			userID, b.TokenID, b.Address); err != nil {
			return err
		}
		imported = true
		return equipNFTSlots(ctx, tx, userID, slots)
	})
	return imported && err == nil, err
}

func (s *Store) NFTCards(ctx context.Context) ([]collectible.Card, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,name,english,chinese,picture,address,status FROM nft_card_inventory`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (collectible.Card, error) {
		var card collectible.Card
		err := row.Scan(&card.ID, &card.Name, &card.English, &card.Chinese, &card.Picture, &card.Address, &card.Status)
		return card, err
	})
}

func (s *Store) SaveNFTCards(ctx context.Context, cards []collectible.Card, sold bool) error {
	if len(cards) == 0 {
		return nil
	}
	return s.nftChange(ctx, func(tx pgx.Tx) error {
		for _, card := range cards {
			if _, err := tx.Exec(ctx, `INSERT INTO nft_card_inventory(id,name,english,chinese,picture,address,status,sold_at)
				VALUES($1,$2,$3,$4,$5,$6,$7,CASE WHEN $8 THEN statement_timestamp() END)
				ON CONFLICT(id) DO UPDATE SET address=EXCLUDED.address,status=EXCLUDED.status,sold_at=COALESCE(nft_card_inventory.sold_at,EXCLUDED.sold_at)
				WHERE $8 AND (nft_card_inventory.status<>1 OR nft_card_inventory.address='')`, card.ID, card.Name, card.English, card.Chinese, card.Picture,
				card.Address, card.Status, sold); err != nil {
				return err
			}
		}
		return nil
	})
}
