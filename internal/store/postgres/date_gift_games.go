package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/date"
	"open_live_platform/internal/gift"
)

var _ date.GiftGameStore = (*Store)(nil)

func findGiftGame(ctx context.Context, tx pgx.Tx, channel int64) (date.GiftGame, error) {
	var game date.GiftGame
	err := tx.QueryRow(ctx, `SELECT game_data FROM date_gift_games WHERE channel=$1`, channel).Scan(&game)
	if errors.Is(err, pgx.ErrNoRows) {
		err = nil
	}
	return game, err
}

func saveGiftGame(ctx context.Context, tx pgx.Tx, game date.GiftGame) error {
	_, err := tx.Exec(ctx, `INSERT INTO date_gift_games(channel,game_data) VALUES($1,$2)
		ON CONFLICT(channel) DO UPDATE SET game_data=EXCLUDED.game_data`, game.Channel, game)
	return err
}

func activeGiftGameSession(ctx context.Context, tx pgx.Tx, channel, actor int64) (date.Session, error) {
	session, err := loadDateSession(ctx, tx, channel)
	if err != nil {
		return session, err
	}
	if actor != 0 && !session.Includes(actor) {
		return session, date.ErrForbidden
	}
	if !session.Active(time.Now()) {
		return session, date.ErrClosed
	}
	return session, nil
}

func findUnclaimedGameOrder(ctx context.Context, tx pgx.Tx, channel, giftID int64, waitingOnly bool) (date.GiftOrder, error) {
	return readDateGift(tx.QueryRow(ctx, `SELECT o.order_data FROM date_gift_orders o
		WHERE o.channel=$1 AND (o.order_data->'Input'->>'giftId')::bigint=$2
		  AND (o.state=3 OR (NOT $3 AND o.state IN(0,1))) AND NOT EXISTS
			(SELECT 1 FROM date_gift_game_orders d WHERE d.order_id=o.id)
		ORDER BY CASE o.state WHEN 3 THEN 0 WHEN 0 THEN 1 ELSE 2 END,o.id DESC LIMIT 1`, channel, giftID, waitingOnly))
}

func (s *Store) PrepareDateGiftGame(ctx context.Context, channel, actor int64) error {
	return s.dateChange(ctx, func(tx pgx.Tx) error {
		session, err := activeGiftGameSession(ctx, tx, channel, actor)
		if err != nil {
			return err
		}
		if session.AudienceID != actor {
			return date.ErrForbidden
		}
		// A verified request can start a new round after a terminal result.
		// Order claims remain recorded so payment replays cannot be reused.
		_, err = tx.Exec(ctx, `DELETE FROM date_gift_games WHERE channel=$1 AND (game_data->>'Stage')::int IN ($2,$3)`,
			channel, date.GameFinished, date.GameRejected)
		return err
	})
}

func (s *Store) VoteDateGiftGame(ctx context.Context, candidate date.GiftOrder, actor int64, kind uint8,
	consent bool, questions []gift.Question) (result date.GiftGame, changed bool, err error) {
	err = s.dateChange(ctx, func(tx pgx.Tx) error {
		session, err := activeGiftGameSession(ctx, tx, candidate.Input.Channel, actor)
		if err != nil {
			return err
		}
		if candidate.Item.GiftType != 1 || kind > 1 || candidate.Input.Sender != session.AudienceID || candidate.Input.Receiver != session.AnchorID {
			return date.ErrForbidden
		}
		now := time.Now().UTC()
		game, err := findGiftGame(ctx, tx, session.Channel)
		if err != nil {
			return err
		}
		if game.ID != 0 && now.Before(game.Expires) {
			if game.Stage == date.GameFinished || game.Stage == date.GameRejected {
				_, findErr := findUnclaimedGameOrder(ctx, tx, session.Channel, candidate.Item.ID, true)
				if errors.Is(findErr, gift.ErrOrderNotFound) {
					result = game
					if game.GiftID != candidate.Item.ID || game.Kind != kind {
						return date.ErrGameNotReady
					}
					return nil
				}
				if findErr != nil {
					return findErr
				}
				game = date.GiftGame{}
			}
		} else {
			game = date.GiftGame{}
		}
		if game.ID == 0 {
			game = date.GiftGame{ID: date.NewChannel(), Channel: session.Channel, GiftID: candidate.Item.ID, Kind: kind,
				Anchor: session.AnchorID, Audience: session.AudienceID, Expires: now.Add(date.GiftGameLifetime), Questions: questions}
		}
		if game.GiftID != candidate.Item.ID || game.Kind != kind {
			return date.ErrGameBusy
		}
		if game.Stage != date.GameVoting {
			result = game
			return nil
		}
		changed = game.Vote(actor, consent)
		if changed {
			if game.Stage == date.GamePayment && game.Kind == 1 && len(game.Questions) != 5 {
				if len(questions) != 5 {
					return date.ErrGameQuestions
				}
				game.Questions = questions
			}
			if err := claimGiftGameOrder(ctx, tx, &game, candidate); err != nil {
				return err
			}
		}
		result = game
		return saveGiftGame(ctx, tx, game)
	})
	return result, changed, err
}

func claimGiftGameOrder(ctx context.Context, tx pgx.Tx, game *date.GiftGame, candidate date.GiftOrder) error {
	if game.Stage == date.GamePayment {
		blocked, err := dateGiftBlocked(ctx, tx, game.Channel, game.GiftID)
		if err != nil {
			return err
		}
		if blocked {
			return gift.ErrBlocked
		}
	}
	order, err := findUnclaimedGameOrder(ctx, tx, game.Channel, game.GiftID, game.Stage == date.GameRejected)
	if err != nil && !errors.Is(err, gift.ErrOrderNotFound) {
		return err
	}
	if errors.Is(err, gift.ErrOrderNotFound) {
		if game.Stage == date.GameRejected {
			return nil
		}
		// Refuse to displace a different gift already waiting in this date.
		var waiting bool
		err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM date_gift_orders WHERE channel=$1 AND state=3)`, game.Channel).Scan(&waiting)
		if err != nil {
			return err
		}
		if waiting {
			return date.ErrGameWaiting
		}
		order = candidate
		if err := verifyGiftCatalogQuote(ctx, tx, order.Item); err != nil {
			return err
		}
		order.State = date.GiftWaiting
		if _, err := tx.Exec(ctx, `INSERT INTO date_gift_orders(id,channel,anchor_id,sender_id,request_key,state,order_data)
			VALUES($1,$2,$3,$4,$5,$6,$7)`, order.ID, game.Channel, game.Anchor, game.Audience, order.Key, order.State, order); err != nil {
			return err
		}
	}
	if order.Item.GiftType != 1 || order.Input.Quantity != 1 || order.Input.Receiver != game.Anchor || order.Input.Sender != game.Audience {
		return gift.ErrOrderConflict
	}
	game.OrderID = order.ID
	if _, err := tx.Exec(ctx, `INSERT INTO date_gift_game_orders(order_id,game_id) VALUES($1,$2)`, order.ID, game.ID); err != nil {
		return err
	}
	if game.Stage == date.GameRejected {
		order.State = date.GiftRefused
		_, err := tx.Exec(ctx, `UPDATE date_gift_orders SET state=$2,order_data=$3 WHERE id=$1`, order.ID, order.State, order)
		return err
	}
	return nil
}

func (s *Store) BeginPaidGiftGame(ctx context.Context, channel, gameID int64) (result date.GiftGame, changed bool, err error) {
	err = s.dateChange(ctx, func(tx pgx.Tx) error {
		if _, err := activeGiftGameSession(ctx, tx, channel, 0); err != nil {
			return err
		}
		game, err := findGiftGame(ctx, tx, channel)
		if err != nil {
			return err
		}
		if game.ID != gameID || !time.Now().Before(game.Expires) {
			return date.ErrGameNotReady
		}
		result = game
		if game.Stage != date.GamePayment {
			return nil
		}
		order, err := readDateGift(tx.QueryRow(ctx, `SELECT order_data FROM date_gift_orders WHERE id=$1 AND channel=$2`, game.OrderID, channel))
		if err != nil {
			return err
		}
		switch order.State {
		case date.GiftPaid:
			game.Stage = date.GamePlaying
		case date.GiftDeclined, date.GiftRefused:
			game.Stage, game.RejectedBy = date.GameRejected, 2
		default:
			return gift.ErrPending
		}
		result, changed = game, true
		return saveGiftGame(ctx, tx, game)
	})
	return result, changed, err
}

func (s *Store) MoveGiftGame(ctx context.Context, channel, actor int64, move date.GameMove) (result *date.GameResult, err error) {
	err = s.dateChange(ctx, func(tx pgx.Tx) error {
		if _, err := activeGiftGameSession(ctx, tx, channel, actor); err != nil {
			return err
		}
		game, err := findGiftGame(ctx, tx, channel)
		if err != nil {
			return err
		}
		if game.ID == 0 || !time.Now().Before(game.Expires) {
			return date.ErrGameNotReady
		}
		if game.Stage == date.GameFinished && game.Role(actor) > 0 && game.Kind == move.Kind {
			return nil
		}
		result, err = game.Move(actor, move)
		if err != nil {
			return err
		}
		return saveGiftGame(ctx, tx, game)
	})
	return result, err
}
