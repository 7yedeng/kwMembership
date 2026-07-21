package processor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"kwmembership/internal/domain"
	"kwmembership/internal/provider"
	"kwmembership/internal/store"
)

const (
	inventoryRunLock    = 5 * time.Minute
	inventoryRefresh    = 6 * time.Hour
	inventoryPageSize   = 20
	inventoryMaxPages   = 100
	transactionPageSize = 50
)

type inventoryRun struct {
	ID         string
	Mode       string
	Status     string
	NextPage   int
	Total      sql.NullInt64
	Discovered int
}

type inventoryItem struct {
	RunID          string
	UpstreamCardID int64
	AttemptCount   int
}

func (p *Processor) tickInventory(ctx context.Context) (processed bool, returned error) {
	now := p.now().UTC()
	run, found, err := p.claimInventoryRun(ctx, now)
	if err != nil {
		return false, err
	}
	if !found {
		if scheduled, err := p.scheduleTargetedInventory(ctx, now); err != nil || scheduled {
			return scheduled, err
		}
		return p.schedulePeriodicInventory(ctx, now)
	}
	defer func() {
		if err := p.releaseInventoryRun(context.Background(), run.ID, p.now().UTC()); returned == nil && err != nil {
			returned = err
		}
	}()
	var item inventoryItem
	if run.Status != "discovering" {
		var itemFound bool
		item, itemFound, err = p.nextInventoryItem(ctx, run.ID, now)
		if err != nil {
			return false, err
		}
		if !itemFound {
			var remaining int
			if err := p.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM card_inventory_run_items WHERE run_id=? AND status='pending'`, run.ID).Scan(&remaining); err != nil {
				return false, err
			}
			if remaining == 0 {
				return true, p.updateInventoryProgress(ctx, run.ID, now)
			}
			return false, nil
		}
	}
	allowed, err := p.acquireCircuit(ctx, "spacexcard_openapi", "default", now)
	if err != nil {
		return false, err
	}
	if !allowed {
		return false, coded("SPACEXCARD_CIRCUIT_OPEN", "SpaceX Card dependency circuit is open")
	}
	client, err := p.spaceXClient(ctx)
	if err != nil {
		persistErr := p.inventoryRunFailure(ctx, run.ID, err, now)
		circuitErr := p.recordCircuitFailure(ctx, "spacexcard_openapi", "default", err, now)
		if persistErr != nil {
			return true, withCircuitAccounting(persistErr, circuitErr)
		}
		return true, withCircuitAccounting(err, circuitErr)
	}
	if run.Status == "discovering" {
		if err := p.discoverInventoryPage(ctx, client, run, now); err != nil {
			if leaseErr := p.assertWorkAllowed(ctx); leaseErr != nil {
				return true, leaseErr
			}
			persistErr := p.inventoryRunFailure(ctx, run.ID, err, now)
			circuitErr := p.recordCircuitFailure(ctx, "spacexcard_openapi", "default", err, now)
			if persistErr != nil {
				return true, withCircuitAccounting(persistErr, circuitErr)
			}
			return true, withCircuitAccounting(err, circuitErr)
		}
		return true, withCircuitAccounting(nil,
			p.recordCircuitSuccess(ctx, "spacexcard_openapi", "default", now))
	}
	if err := p.reconcileInventoryCard(ctx, client, run, item, now); err != nil {
		if leaseErr := p.assertWorkAllowed(ctx); leaseErr != nil {
			return true, leaseErr
		}
		persistErr := p.recordInventoryItemFailure(ctx, run, item, err, now)
		circuitErr := p.recordCircuitFailure(ctx, "spacexcard_openapi", "default", err, now)
		if persistErr != nil {
			return true, withCircuitAccounting(persistErr, circuitErr)
		}
		return true, withCircuitAccounting(err, circuitErr)
	}
	return true, withCircuitAccounting(nil,
		p.recordCircuitSuccess(ctx, "spacexcard_openapi", "default", now))
}

func (p *Processor) claimInventoryRun(ctx context.Context, now time.Time) (inventoryRun, bool, error) {
	var run inventoryRun
	found := false
	err := p.withFencedImmediate(ctx, func(tx *sql.Tx) error {
		var id string
		err := tx.QueryRowContext(ctx, `SELECT id FROM card_inventory_runs
      WHERE status IN ('discovering','reconciling')
        AND (locked_at IS NULL OR locked_at<? OR locked_by=?) ORDER BY started_at LIMIT 1`,
			store.ISO(now.Add(-inventoryRunLock)), p.lease.Token).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE card_inventory_runs SET locked_at=?,locked_by=?,updated_at=?
      WHERE id=? AND status IN ('discovering','reconciling')
        AND (locked_at IS NULL OR locked_at<? OR locked_by=?)`, store.ISO(now), p.lease.Token, store.ISO(now), id,
			store.ISO(now.Add(-inventoryRunLock)), p.lease.Token)
		if err != nil {
			return err
		}
		count, _ := result.RowsAffected()
		if count != 1 {
			return nil
		}
		if err := tx.QueryRowContext(ctx, `SELECT id,mode,status,next_page,total_cards,discovered_cards FROM card_inventory_runs WHERE id=?`, id).
			Scan(&run.ID, &run.Mode, &run.Status, &run.NextPage, &run.Total, &run.Discovered); err != nil {
			return err
		}
		found = true
		return nil
	})
	return run, found, err
}

func (p *Processor) releaseInventoryRun(ctx context.Context, id string, now time.Time) error {
	_, err := p.fencedExec(ctx, `UPDATE card_inventory_runs SET locked_at=NULL,locked_by=NULL,updated_at=?
    WHERE id=? AND locked_by=?`, store.ISO(now), id, p.lease.Token)
	return err
}

func (p *Processor) scheduleTargetedInventory(ctx context.Context, now time.Time) (bool, error) {
	scheduled := false
	err := p.withFencedImmediate(ctx, func(tx *sql.Tx) error {
		var status string
		if err := tx.QueryRowContext(ctx, `SELECT inventory_status FROM membership_fulfillment_settings WHERE id='default'`).Scan(&status); err != nil {
			return err
		}
		if status != "completed" {
			return nil
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM card_inventory_runs WHERE status IN ('discovering','reconciling')`).Scan(&count); err != nil || count > 0 {
			return err
		}
		var upstreamID int64
		err := tx.QueryRowContext(ctx, `SELECT upstream_card_id FROM managed_cards
      WHERE reconciliation_state='PENDING' AND reconciliation_reason='WEBHOOK_RECHECK_PENDING'
      ORDER BY updated_at,upstream_card_id LIMIT 1`).Scan(&upstreamID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		id, err := store.NewID("mir_target_")
		if err != nil {
			return err
		}
		at := store.ISO(now)
		if _, err = tx.ExecContext(ctx, `INSERT INTO card_inventory_runs
      (id,mode,status,next_page,total_cards,discovered_cards,processed_cards,held_cards,started_at,updated_at)
      VALUES (?,'targeted','reconciling',1,1,1,0,0,?,?)`, id, at, at); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO card_inventory_run_items
      (run_id,upstream_card_id,status,attempt_count,updated_at) VALUES (?,?,'pending',0,?)`, id, upstreamID, at); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE membership_fulfillment_settings SET inventory_status='running',
      last_inventory_error=NULL,updated_at=?,updated_by='go-worker' WHERE id='default'`, at)
		scheduled = err == nil
		return err
	})
	return scheduled, err
}

func (p *Processor) schedulePeriodicInventory(ctx context.Context, now time.Time) (bool, error) {
	var status string
	var encrypted sql.NullString
	if err := p.store.DB().QueryRowContext(ctx, `SELECT inventory_status,spacexcard_app_secret_encrypted
    FROM membership_fulfillment_settings WHERE id='default'`).Scan(&status, &encrypted); err != nil {
		return false, err
	}
	if status != "completed" || !encrypted.Valid || encrypted.String == "" {
		return false, nil
	}
	var completed sql.NullString
	if err := p.store.DB().QueryRowContext(ctx, `SELECT completed_at FROM card_inventory_runs
    WHERE mode IN ('full','refresh') AND status='completed' ORDER BY completed_at DESC LIMIT 1`).Scan(&completed); errors.Is(err, sql.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	parsed, err := time.Parse(time.RFC3339Nano, completed.String)
	if err != nil || now.Sub(parsed) < inventoryRefresh {
		return false, nil
	}
	scheduled := false
	err = p.withFencedImmediate(ctx, func(tx *sql.Tx) error {
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM card_inventory_runs WHERE status IN ('discovering','reconciling')`).Scan(&active); err != nil || active > 0 {
			return err
		}
		id, err := store.NewID("mir_")
		if err != nil {
			return err
		}
		at := store.ISO(now)
		if _, err = tx.ExecContext(ctx, `INSERT INTO card_inventory_runs
      (id,mode,status,next_page,total_cards,discovered_cards,processed_cards,held_cards,started_at,updated_at)
      VALUES (?,'refresh','discovering',1,NULL,0,0,0,?,?)`, id, at, at); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE membership_fulfillment_settings SET inventory_status='running',last_inventory_error=NULL,
      updated_at=?,updated_by='go-worker' WHERE id='default'`, at)
		scheduled = err == nil
		return err
	})
	return scheduled, err
}

func (p *Processor) discoverInventoryPage(ctx context.Context, client *provider.SpaceXClient, run inventoryRun, now time.Time) error {
	if run.NextPage < 1 || run.NextPage > inventoryMaxPages {
		return coded("SPACEXCARD_CONTRACT_DRIFT", "SpaceX Card inventory pagination exceeded its safe limit")
	}
	total, cards, err := client.ListCards(ctx, run.NextPage, inventoryPageSize, true)
	if err != nil {
		return err
	}
	if err := p.assertWorkAllowed(ctx); err != nil {
		return err
	}
	return p.withFencedImmediate(ctx, func(tx *sql.Tx) error {
		if run.Total.Valid && run.Total.Int64 != int64(total) {
			return coded("SPACEXCARD_CONTRACT_DRIFT", "SpaceX Card inventory total changed during pagination")
		}
		at := store.ISO(now)
		for _, card := range cards {
			id := fmt.Sprintf("mc_%d", card.UpstreamCardID)
			if _, err := tx.ExecContext(ctx, `INSERT INTO managed_cards
        (id,upstream_card_id,vm_card_id,product_code,bin,last4,upstream_status,cached_available_amount,
         capacity_state,reconciliation_state,last_balance_sync_at,created_at,updated_at)
        VALUES (?,?,?,?,?,?,?,?,'PENDING','PENDING',?,?,?)
        ON CONFLICT(upstream_card_id) DO UPDATE SET vm_card_id=excluded.vm_card_id,product_code=excluded.product_code,
          bin=excluded.bin,last4=excluded.last4,upstream_status=excluded.upstream_status,
          cached_available_amount=excluded.cached_available_amount,last_balance_sync_at=excluded.last_balance_sync_at,
          updated_at=excluded.updated_at`, id, card.UpstreamCardID, card.VMCardID, card.ProductCode,
				nullString(card.BIN), nullString(card.Last4), card.Status, card.AvailableAmount, at, at, at); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO card_inventory_run_items
        (run_id,upstream_card_id,status,attempt_count,updated_at) VALUES (?,?,'pending',0,?)`, run.ID, card.UpstreamCardID, at); err != nil {
				return err
			}
		}
		var discovered int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM card_inventory_run_items WHERE run_id=?`, run.ID).Scan(&discovered); err != nil {
			return err
		}
		if discovered > total || (len(cards) > 0 && discovered == run.Discovered) ||
			(len(cards) == 0 && discovered < total) || (len(cards) < inventoryPageSize && discovered < total) {
			return coded("SPACEXCARD_CONTRACT_DRIFT", "SpaceX Card inventory pagination is inconsistent")
		}
		done := discovered == total
		status := "discovering"
		if done {
			status = "reconciling"
		}
		_, err := tx.ExecContext(ctx, `UPDATE card_inventory_runs SET status=?,next_page=next_page+1,total_cards=?,
      discovered_cards=?,last_error_code=NULL,updated_at=? WHERE id=?`, status, total, discovered, at, run.ID)
		return err
	})
}

func (p *Processor) nextInventoryItem(ctx context.Context, runID string, now time.Time) (inventoryItem, bool, error) {
	var item inventoryItem
	err := p.store.DB().QueryRowContext(ctx, `SELECT run_id,upstream_card_id,attempt_count FROM card_inventory_run_items
    WHERE run_id=? AND status='pending' AND (next_retry_at IS NULL OR next_retry_at<=?)
    ORDER BY upstream_card_id LIMIT 1`, runID, store.ISO(now)).Scan(&item.RunID, &item.UpstreamCardID, &item.AttemptCount)
	if errors.Is(err, sql.ErrNoRows) {
		return inventoryItem{}, false, nil
	}
	return item, err == nil, err
}

func (p *Processor) loadAllTransactions(ctx context.Context, client *provider.SpaceXClient, upstreamID int64) ([]provider.Transaction, error) {
	var all []provider.Transaction
	for page := 1; page <= 100; page++ {
		items, err := client.ListTransactions(ctx, upstreamID, page, transactionPageSize)
		if err != nil {
			return nil, err
		}
		all = append(all, items...)
		if len(items) < transactionPageSize {
			return all, nil
		}
	}
	return nil, coded("CARD_TRANSACTION_PAGINATION_EXCEEDED", "card transactions exceed pagination limit")
}

type collapsedTransaction struct {
	Transaction                                                 domain.CardTransaction
	AuthorizationSeen, SettlementSeen, RefundSeen, ReversalSeen int
}

func collapseProviderTransactions(items []provider.Transaction) []collapsedTransaction {
	byID := map[string]collapsedTransaction{}
	order := []string{}
	for _, item := range items {
		authCents, authErr := domain.CentsFromUSD(item.AuthAmount)
		settleCents, settleErr := domain.CentsFromUSD(item.SettleAmount)
		tx := domain.CardTransaction{AuthID: item.AuthID, AuthTime: item.AuthTime, CreatedAt: item.CreatedAt,
			AuthCurrency: item.AuthCurrency, SettleCurrency: item.SettleCurrency, MerchantNormalized: item.MerchantNormalized,
			Type: item.Type, Status: item.Status}
		if authErr == nil {
			tx.AuthAmountCents = &authCents
		}
		if settleErr == nil {
			tx.SettleAmountCents = &settleCents
		}
		current, exists := byID[item.AuthID]
		if !exists {
			current.Transaction = tx
			order = append(order, item.AuthID)
		}
		if current.Transaction.AuthTime == "" {
			current.Transaction.AuthTime = tx.AuthTime
		}
		if tx.AuthAmountCents != nil && *tx.AuthAmountCents > 0 {
			current.Transaction.AuthAmountCents = tx.AuthAmountCents
		}
		if tx.AuthCurrency != "" {
			current.Transaction.AuthCurrency = tx.AuthCurrency
		}
		if tx.SettleAmountCents != nil && *tx.SettleAmountCents > 0 {
			current.Transaction.SettleAmountCents = tx.SettleAmountCents
		}
		if tx.SettleCurrency != "" {
			current.Transaction.SettleCurrency = tx.SettleCurrency
		}
		if tx.MerchantNormalized == "OPENAI" {
			current.Transaction.MerchantNormalized = "OPENAI"
		}
		switch tx.Type {
		case domain.TransactionTypeAuthorization:
			current.AuthorizationSeen = 1
		case domain.TransactionTypeSettlement:
			current.SettlementSeen = 1
		case domain.TransactionTypeRefund:
			current.RefundSeen = 1
		case domain.TransactionTypeReversal:
			current.ReversalSeen = 1
		}
		state := domain.SelectCanonicalCardTransactionState(domain.TransactionState{Type: current.Transaction.Type, Status: current.Transaction.Status}, domain.TransactionState{Type: tx.Type, Status: tx.Status})
		current.Transaction.Type, current.Transaction.Status = state.Type, state.Status
		byID[item.AuthID] = current
	}
	result := make([]collapsedTransaction, 0, len(order))
	for _, id := range order {
		result = append(result, byID[id])
	}
	return result
}

func providerDomainTransactions(items []provider.Transaction) []domain.CardTransaction {
	result := make([]domain.CardTransaction, 0, len(items))
	for _, item := range items {
		auth, authErr := domain.CentsFromUSD(item.AuthAmount)
		settle, settleErr := domain.CentsFromUSD(item.SettleAmount)
		tx := domain.CardTransaction{AuthID: item.AuthID, AuthTime: item.AuthTime, CreatedAt: item.CreatedAt, AuthCurrency: item.AuthCurrency,
			SettleCurrency: item.SettleCurrency, MerchantNormalized: item.MerchantNormalized, Type: item.Type, Status: item.Status}
		if authErr == nil {
			tx.AuthAmountCents = &auth
		}
		if settleErr == nil {
			tx.SettleAmountCents = &settle
		}
		result = append(result, tx)
	}
	return result
}

func (p *Processor) reconcileInventoryCard(ctx context.Context, client *provider.SpaceXClient, run inventoryRun, item inventoryItem, now time.Time) error {
	var cardID string
	var lane sql.NullString
	if err := p.store.DB().QueryRowContext(ctx, `SELECT id,lane FROM managed_cards WHERE upstream_card_id=?`, item.UpstreamCardID).Scan(&cardID, &lane); err != nil {
		return codedWrap("MANAGED_CARD_NOT_FOUND", "managed card missing", err)
	}
	transactions, err := p.loadAllTransactions(ctx, client, item.UpstreamCardID)
	if err != nil {
		return err
	}
	prices, err := client.GetOpenAIPayments(ctx, item.UpstreamCardID)
	if err != nil {
		return err
	}
	if err := p.assertWorkAllowed(ctx); err != nil {
		return err
	}
	knownLane := domain.Tier("")
	if lane.Valid {
		knownLane = domain.Tier(lane.String)
	} else {
		rows, err := p.store.DB().QueryContext(ctx, `SELECT DISTINCT target_lane FROM card_capacity_reservations
      WHERE card_id=? AND state IN ('reserved','consumed','retained_partial')`, cardID)
		if err != nil {
			return err
		}
		var lanes []string
		for rows.Next() {
			var value string
			if err := rows.Scan(&value); err != nil {
				rows.Close()
				return err
			}
			lanes = append(lanes, value)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if len(lanes) == 1 {
			knownLane = domain.Tier(lanes[0])
		}
	}
	classification := domain.ClassifyHistoricalCardFulfillments(providerDomainTransactions(transactions), knownLane)
	collapsed := collapseProviderTransactions(transactions)
	held := classification.State == domain.HistoricalStateReconciliationHold
	return p.withFencedImmediate(ctx, func(tx *sql.Tx) error {
		at := store.ISO(now)
		for _, item := range collapsed {
			t := item.Transaction
			var authAmount, settleAmount any
			if t.AuthAmountCents != nil {
				authAmount = domain.USDFromCents(*t.AuthAmountCents)
			}
			if t.SettleAmountCents != nil {
				settleAmount = domain.USDFromCents(*t.SettleAmountCents)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO managed_card_transactions
        (card_id,auth_id,auth_time,auth_amount,auth_currency,settle_amount,settle_currency,type,status,merchant_normalized,
         authorization_seen,settlement_seen,refund_seen,reversal_seen,first_seen_at,last_seen_at)
        VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(card_id,auth_id) DO UPDATE SET
          auth_time=COALESCE(excluded.auth_time,managed_card_transactions.auth_time),
          auth_amount=CASE WHEN excluded.auth_amount>0 THEN excluded.auth_amount ELSE managed_card_transactions.auth_amount END,
          auth_currency=COALESCE(excluded.auth_currency,managed_card_transactions.auth_currency),
          settle_amount=CASE WHEN excluded.settle_amount>0 THEN excluded.settle_amount ELSE managed_card_transactions.settle_amount END,
          settle_currency=COALESCE(excluded.settle_currency,managed_card_transactions.settle_currency),type=excluded.type,status=excluded.status,
          merchant_normalized=CASE WHEN excluded.merchant_normalized='OPENAI' THEN 'OPENAI' ELSE managed_card_transactions.merchant_normalized END,
          authorization_seen=MAX(managed_card_transactions.authorization_seen,excluded.authorization_seen),
          settlement_seen=MAX(managed_card_transactions.settlement_seen,excluded.settlement_seen),refund_seen=MAX(managed_card_transactions.refund_seen,excluded.refund_seen),
          reversal_seen=MAX(managed_card_transactions.reversal_seen,excluded.reversal_seen),last_seen_at=excluded.last_seen_at`, cardID, t.AuthID, nullString(t.AuthTime), authAmount, nullString(t.AuthCurrency), settleAmount, nullString(t.SettleCurrency), t.Type, t.Status, t.MerchantNormalized, item.AuthorizationSeen, item.SettlementSeen, item.RefundSeen, item.ReversalSeen, at, at); err != nil {
				return err
			}
		}
		for _, price := range prices {
			if _, err := tx.ExecContext(ctx, `INSERT INTO card_price_signals
        (card_id,tier,found,amount,min_usd,max_usd,provider_time,fetched_at) VALUES (?,?,?,?,?,?,?,?)
        ON CONFLICT(card_id,tier) DO UPDATE SET found=excluded.found,amount=excluded.amount,min_usd=excluded.min_usd,
        max_usd=excluded.max_usd,provider_time=excluded.provider_time,fetched_at=excluded.fetched_at`, cardID, price.Tier, boolInt(price.Found), price.Amount, price.MinUSD, price.MaxUSD, nullString(price.Time), at); err != nil {
				return err
			}
		}
		var finalLane any
		if classification.Lane != nil {
			finalLane = string(*classification.Lane)
		}
		capacityState := string(classification.State)
		reconState := "READY"
		if held {
			capacityState = "HOLD"
			reconState = "HOLD"
		}
		if _, err := tx.ExecContext(ctx, `UPDATE managed_cards SET lane=?,consumed_slots=?,capacity_state=?,reconciliation_state=?,
      reconciliation_reason=?,last_transaction_sync_at=?,updated_at=? WHERE id=?`, finalLane, classification.Consumed, capacityState, reconState, nullString(classification.Reason), at, at, cardID); err != nil {
			return err
		}
		itemStatus := "succeeded"
		if held {
			itemStatus = "held"
		}
		if _, err := tx.ExecContext(ctx, `UPDATE card_inventory_run_items SET status=?,error_code=?,next_retry_at=NULL,updated_at=?
      WHERE run_id=? AND upstream_card_id=?`, itemStatus, nullString(classification.Reason), at, run.ID, item.UpstreamCardID); err != nil {
			return err
		}
		return p.updateInventoryProgressWith(ctx, tx, run.ID, now)
	})
}

func (p *Processor) updateInventoryProgress(ctx context.Context, runID string, now time.Time) error {
	return p.withFencedImmediate(ctx, func(tx *sql.Tx) error { return p.updateInventoryProgressWith(ctx, tx, runID, now) })
}
func (p *Processor) updateInventoryProgressWith(ctx context.Context, tx *sql.Tx, runID string, now time.Time) error {
	var processed, held, pending int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(CASE WHEN status IN ('succeeded','held') THEN 1 ELSE 0 END),0),
    COALESCE(SUM(CASE WHEN status='held' THEN 1 ELSE 0 END),0),COALESCE(SUM(CASE WHEN status='pending' THEN 1 ELSE 0 END),0)
    FROM card_inventory_run_items WHERE run_id=?`, runID).Scan(&processed, &held, &pending); err != nil {
		return err
	}
	at := store.ISO(now)
	if _, err := tx.ExecContext(ctx, `UPDATE card_inventory_runs SET processed_cards=?,held_cards=?,updated_at=? WHERE id=?`, processed, held, at, runID); err != nil {
		return err
	}
	if pending > 0 {
		return nil
	}
	var mode string
	if err := tx.QueryRowContext(ctx, `SELECT mode FROM card_inventory_runs WHERE id=?`, runID).Scan(&mode); err != nil {
		return err
	}
	if mode == "full" || mode == "refresh" {
		if _, err := tx.ExecContext(ctx, `UPDATE managed_cards SET upstream_status='MISSING',capacity_state='HOLD',reconciliation_state='HOLD',
      reconciliation_reason='UPSTREAM_CARD_MISSING',updated_at=? WHERE NOT EXISTS(SELECT 1 FROM card_inventory_run_items item
      WHERE item.run_id=? AND item.upstream_card_id=managed_cards.upstream_card_id)`, at, runID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE card_inventory_runs SET status='completed',completed_at=?,last_error_code=NULL,locked_at=NULL,locked_by=NULL,updated_at=? WHERE id=?`, at, at, runID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE membership_fulfillment_settings SET inventory_status='completed',inventory_initialized_at=COALESCE(inventory_initialized_at,?),
    last_inventory_error=NULL,updated_at=?,updated_by='go-worker' WHERE id='default'`, at, at)
	return err
}

func (p *Processor) recordInventoryItemFailure(ctx context.Context, run inventoryRun, item inventoryItem, cause error, now time.Time) error {
	attempt := item.AttemptCount + 1
	code := errorCode(cause)
	cardSpecific := code == "SPACEXCARD_OPERATION_REJECTED" && attempt >= 3
	delays := []time.Duration{30 * time.Second, 2 * time.Minute, 5 * time.Minute, 15 * time.Minute}
	delay := delays[min(attempt-1, len(delays)-1)]
	return p.withFencedImmediate(ctx, func(tx *sql.Tx) error {
		status := "pending"
		var retry any = store.ISO(now.Add(delay))
		if cardSpecific {
			status = "held"
			retry = nil
		}
		at := store.ISO(now)
		if _, err := tx.ExecContext(ctx, `UPDATE card_inventory_run_items SET status=?,attempt_count=?,next_retry_at=?,error_code=?,updated_at=? WHERE run_id=? AND upstream_card_id=?`, status, attempt, retry, code, at, run.ID, item.UpstreamCardID); err != nil {
			return err
		}
		if cardSpecific {
			if _, err := tx.ExecContext(ctx, `UPDATE managed_cards SET capacity_state='HOLD',reconciliation_state='HOLD',reconciliation_reason='CARD_SYNC_REJECTED',updated_at=? WHERE upstream_card_id=?`, at, item.UpstreamCardID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE card_inventory_runs SET last_error_code=?,updated_at=? WHERE id=?`, code, at, run.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE membership_fulfillment_settings SET last_inventory_error=?,updated_at=?,updated_by='go-worker' WHERE id='default'`, code, at); err != nil {
			return err
		}
		return p.updateInventoryProgressWith(ctx, tx, run.ID, now)
	})
}

func (p *Processor) inventoryRunFailure(ctx context.Context, runID string, cause error, now time.Time) error {
	code := errorCode(cause)
	at := store.ISO(now)
	return p.withFencedImmediate(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE card_inventory_runs SET last_error_code=?,updated_at=? WHERE id=?`, code, at, runID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE membership_fulfillment_settings SET last_inventory_error=?,updated_at=?,updated_by='go-worker' WHERE id='default'`, code, at)
		return err
	})
}
