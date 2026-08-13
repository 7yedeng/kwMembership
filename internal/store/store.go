package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

type Execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func Open(path string) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}
	u := &url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}
	query := u.Query()
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "journal_mode(WAL)")
	query.Set("_txlock", "immediate")
	u.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// One Go connection keeps transaction and PRAGMA behavior deterministic;
	// Node's API remains a separate WAL participant.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }
func (s *Store) DB() *sql.DB  { return s.db }

func (s *Store) VerifySharedSchema(ctx context.Context) error {
	for _, table := range []string{
		"redeem_orders",
		"membership_fulfillments",
		"membership_fulfillment_settings",
		"membership_processor_lease",
		"membership_intake_settings",
		"membership_fulfillment_attempts",
		"membership_payment_stages",
		"membership_action_permits",
		"membership_action_auth_snapshots",
		"managed_cards",
		"checkout_price_contracts",
		"checkout_validation_runs",
		"extension_delivery_settings",
		"live_canary_authorizations",
		"automatic_checkout_quota_reservations",
		"automatic_checkout_scopes",
		"membership_checkout_commands",
	} {
		var found string
		if err := s.db.QueryRowContext(ctx,
			"SELECT name FROM sqlite_master WHERE type='table' AND name=?", table,
		).Scan(&found); err != nil {
			if err == sql.ErrNoRows {
				return fmt.Errorf("kwRedeem database is missing table %s", table)
			}
			return err
		}
	}
	return nil
}

func (s *Store) WithImmediate(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}
