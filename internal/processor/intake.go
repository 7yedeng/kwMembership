package processor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"kwmembership/internal/store"
)

type intakeCandidate struct {
	orderID, orderNo, siteID, productID, targetTier, createdAt string
}

// tickIntake is the single intake seam for membership automation. kwRedeem
// only records the order and delivery facts; Go owns every fulfillment state
// from creation onward and waits for successful delivery before eligibility.
func (p *Processor) tickIntake(ctx context.Context) (bool, error) {
	rows, err := p.store.DB().QueryContext(ctx, `
    SELECT o.id, o.order_no, o.site_id, o.product_id,
      COALESCE(
        NULLIF(LOWER(TRIM(c.manual_type)), ''),
        CASE WHEN json_valid(c.metadata)
          THEN NULLIF(LOWER(TRIM(json_extract(c.metadata, '$.manualType'))), '') END,
        NULLIF(LOWER(TRIM(product.membership_tier)), '')
      ) AS target_tier,
      o.created_at
    FROM redeem_orders o
    LEFT JOIN cdkeys c ON c.id = o.cdkey_id
    LEFT JOIN products product ON product.id = o.product_id
    LEFT JOIN membership_fulfillments fulfillment ON fulfillment.order_id = o.id
	JOIN membership_intake_settings intake ON intake.id = 'default'
    WHERE fulfillment.id IS NULL
	  AND o.created_at >= intake.accept_orders_created_at
      AND COALESCE(
        NULLIF(LOWER(TRIM(c.manual_type)), ''),
        CASE WHEN json_valid(c.metadata)
          THEN NULLIF(LOWER(TRIM(json_extract(c.metadata, '$.manualType'))), '') END,
        NULLIF(LOWER(TRIM(product.membership_tier)), '')
      ) IN ('plus', 'x5', 'x20')
    ORDER BY o.created_at
    LIMIT 20`)
	if err != nil {
		return false, err
	}
	var candidates []intakeCandidate
	for rows.Next() {
		var item intakeCandidate
		if err := rows.Scan(&item.orderID, &item.orderNo, &item.siteID, &item.productID, &item.targetTier, &item.createdAt); err != nil {
			rows.Close()
			return false, err
		}
		candidates = append(candidates, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	if err := rows.Close(); err != nil {
		return false, err
	}

	created := false
	for _, candidate := range candidates {
		id, err := store.NewID("mf_")
		if err != nil {
			return created, err
		}
		auditID, err := store.NewID("audit_")
		if err != nil {
			return created, err
		}
		if err := p.withFencedImmediate(ctx, func(tx *sql.Tx) error {
			var existing string
			err := tx.QueryRowContext(ctx,
				"SELECT id FROM membership_fulfillments WHERE order_id = ?", candidate.orderID,
			).Scan(&existing)
			if err == nil {
				return nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}

			var scopeID string
			runMode := any(nil)
			err = tx.QueryRowContext(ctx, `
          SELECT id FROM automatic_checkout_scopes
          WHERE site_id = ? AND product_id = ? AND tier = ? AND status = 'active'
            AND activated_at IS NOT NULL AND activated_at <= ?
          LIMIT 1`, candidate.siteID, candidate.productID, candidate.targetTier, candidate.createdAt).Scan(&scopeID)
			if err == nil {
				runMode = "automatic"
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}

			at := store.ISO(p.now())
			result, err := tx.ExecContext(ctx, `
          INSERT OR IGNORE INTO membership_fulfillments (
            id, order_id, order_no, target_tier, state, current_stage, run_mode,
            account_lock_key, resume_revision, state_revision, created_at, updated_at
          ) VALUES (?, ?, ?, ?, 'WAITING_SESSION_VALIDATION', NULL, ?, NULL, 0, 0, ?, ?)`,
				id, candidate.orderID, candidate.orderNo, candidate.targetTier, runMode, candidate.createdAt, at)
			if err != nil {
				return err
			}
			count, err := result.RowsAffected()
			if err != nil || count != 1 {
				return err
			}
			created = true
			detail, err := json.Marshal(map[string]string{
				"orderNo": candidate.orderNo, "targetTier": candidate.targetTier,
			})
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `
          INSERT INTO admin_audit_logs (
            id, action, actor, resource_type, resource_id, detail, created_at
			  ) VALUES (?, 'membership.fulfillment.auto_create', 'go',
				    'membership_fulfillment', ?, ?, ?)`,
				auditID, id, string(detail), at)
			return err
		}); err != nil {
			return created, err
		}
	}
	return created, nil
}
