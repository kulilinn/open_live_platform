package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/admin"
	"open_live_platform/internal/anchor"
)

type LegacyMCNContract struct {
	ID, Agency, Streamer                          int64
	Manager                                       *int64
	ManagerName                                   *string
	Email, Name, Ratio, ManagerRatio, AgencyRatio string
	CreatedAt, JoinedAt                           time.Time
}

type LegacyMCNSettlementProof struct {
	ID                 int64
	ProviderID         *int64
	ProviderNo         *string
	ProviderPoints     *int64
	GrossLD, BadDebtLD int64
	Pending            bool
	At                 time.Time
}

func (s *Store) ImportLegacyMCNContract(ctx context.Context, contract LegacyMCNContract, items []LegacyMCNSettlementProof) error {
	if contract.ID <= 0 || contract.Agency <= 0 || contract.Streamer <= 0 || contract.JoinedAt.IsZero() {
		return admin.ErrMCNReconcile
	}
	return s.mcnChange(ctx, func(tx pgx.Tx) error {
		profile, _, err := anchorForUpdate(ctx, tx, contract.Streamer, false)
		if errors.Is(err, anchor.ErrNotFound) || err == nil && profile.State != anchor.Approved {
			return admin.ErrBackofficeNotFound
		}
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO backoffice_mcn_streamers AS old(id,mcn_id,streamer_id,manager_id,manager_name,email,name,ratio,
			manager_ratio,agency_ratio,status,create_time,joined_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8::text::numeric,$9::text::numeric,
				$10::text::numeric,$11,$12,$13)
			ON CONFLICT(mcn_id,streamer_id) DO UPDATE SET joined_at=EXCLUDED.joined_at
			WHERE old.id=EXCLUDED.id AND old.status=EXCLUDED.status AND NOT old.superseded
				AND (old.joined_at IS NULL OR old.joined_at=EXCLUDED.joined_at)`, contract.ID, contract.Agency, contract.Streamer,
			contract.Manager, contract.ManagerName, contract.Email, contract.Name, contract.Ratio, contract.ManagerRatio, contract.AgencyRatio,
			admin.MCNJoinFinish, contract.CreatedAt, contract.JoinedAt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return admin.ErrMCNState
		}
		tag, err = tx.Exec(ctx, `UPDATE anchor_profiles SET agency_id=$2 WHERE user_id=$1 AND agency_id IN(0,$2)`,
			contract.Streamer, contract.Agency)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return admin.ErrMCNState
		}
		for _, item := range items {
			if item.At.Before(contract.JoinedAt) || item.ID <= 0 || item.GrossLD < 0 || item.BadDebtLD < 0 || item.BadDebtLD > item.GrossLD {
				return admin.ErrMCNReconcile
			}
			verified := item.ProviderID != nil && *item.ProviderID > 0 && item.ProviderNo != nil && *item.ProviderNo != "" &&
				item.ProviderPoints != nil && *item.ProviderPoints > 0
			tag, err := tx.Exec(ctx, `INSERT INTO mcn_legacy_settlement_proofs AS old(settlement_id,agency_id,streamer_id,
				provider_order_id,provider_order_no,charged,gross_ld,bad_debt_ld,provider_verified,source_pending,paid_at)
				VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
				ON CONFLICT(settlement_id) DO UPDATE SET provider_order_id=EXCLUDED.provider_order_id,
				provider_order_no=EXCLUDED.provider_order_no,charged=EXCLUDED.charged,provider_verified=EXCLUDED.provider_verified,
				source_pending=EXCLUDED.source_pending
				WHERE old.agency_id=EXCLUDED.agency_id AND old.streamer_id=EXCLUDED.streamer_id AND old.gross_ld=EXCLUDED.gross_ld
				AND old.bad_debt_ld=EXCLUDED.bad_debt_ld AND old.paid_at=EXCLUDED.paid_at`, item.ID, contract.Agency, contract.Streamer,
				item.ProviderID, item.ProviderNo, item.ProviderPoints, item.GrossLD, item.BadDebtLD, verified, item.Pending, item.At)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return admin.ErrMCNReconcile
			}
		}
		return nil
	})
}
