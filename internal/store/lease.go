package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var ErrLeaseHeld = errors.New("membership processor lease is held by another executor")
var ErrLeaseLost = errors.New("membership processor lease was lost")

type Lease struct {
	Owner string
	Token string
	Epoch int64
}

func (s *Store) EnsureLeaseTable(ctx context.Context, now time.Time) error {
	at := ISO(now)
	_, err := s.db.ExecContext(ctx, `
    CREATE TABLE IF NOT EXISTS membership_processor_lease (
      id TEXT PRIMARY KEY,
      owner TEXT,
      holder_token TEXT,
      epoch INTEGER NOT NULL DEFAULT 0,
      status TEXT NOT NULL DEFAULT 'stopped',
      version TEXT,
      started_at TEXT,
      heartbeat_at TEXT,
      expires_at TEXT,
      last_tick_at TEXT,
      last_success_at TEXT,
      last_error_code TEXT,
      updated_at TEXT NOT NULL
    )`)
	if err != nil {
		return fmt.Errorf("create processor lease: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
    INSERT OR IGNORE INTO membership_processor_lease (id, epoch, status, updated_at)
    VALUES ('default', 0, 'stopped', ?)`, at)
	return err
}

func (s *Store) AcquireLease(ctx context.Context, owner, token, version string, now time.Time, ttl time.Duration) (Lease, error) {
	var lease Lease
	err := s.WithImmediate(ctx, func(tx *sql.Tx) error {
		at, expires := ISO(now), ISO(now.Add(ttl))
		result, err := tx.ExecContext(ctx, `
      UPDATE membership_processor_lease
      SET owner = ?, holder_token = ?, epoch = epoch + 1, status = 'active', version = ?,
          started_at = ?, heartbeat_at = ?, expires_at = ?, last_error_code = NULL, updated_at = ?
      WHERE id = 'default'
        AND (holder_token IS NULL OR expires_at IS NULL OR expires_at <= ? OR (owner = ? AND holder_token = ?))`,
			owner, token, version, at, at, expires, at, at, owner, token)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return ErrLeaseHeld
		}
		if err := tx.QueryRowContext(ctx, `
      SELECT owner, holder_token, epoch FROM membership_processor_lease WHERE id = 'default'`).
			Scan(&lease.Owner, &lease.Token, &lease.Epoch); err != nil {
			return err
		}
		return nil
	})
	return lease, err
}

func (s *Store) HeartbeatLease(ctx context.Context, lease Lease, status string, now time.Time, ttl time.Duration) error {
	result, err := s.db.ExecContext(ctx, `
    UPDATE membership_processor_lease
    SET status = ?, heartbeat_at = ?, expires_at = ?, updated_at = ?
    WHERE id = 'default' AND owner = ? AND holder_token = ? AND epoch = ? AND expires_at > ?`,
		status, ISO(now), ISO(now.Add(ttl)), ISO(now), lease.Owner, lease.Token, lease.Epoch, ISO(now))
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrLeaseLost
	}
	return nil
}

func (s *Store) AssertLease(ctx context.Context, lease Lease, now time.Time) error {
	return assertLease(ctx, s.db, lease, now)
}

// AssertLeaseTx validates the processor epoch on the transaction that is about
// to mutate business state. Because Store opens IMMEDIATE transactions, a
// successful assertion and the following writes are serialized against lease
// takeover until commit.
func (s *Store) AssertLeaseTx(ctx context.Context, tx *sql.Tx, lease Lease, now time.Time) error {
	return assertLease(ctx, tx, lease, now)
}

func assertLease(ctx context.Context, query Execer, lease Lease, now time.Time) error {
	var exists int
	err := query.QueryRowContext(ctx, `
    SELECT 1 FROM membership_processor_lease
    WHERE id = 'default' AND owner = ? AND holder_token = ? AND epoch = ?
      AND status = 'active' AND expires_at > ?`,
		lease.Owner, lease.Token, lease.Epoch, ISO(now)).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrLeaseLost
	}
	return err
}

func (s *Store) RecordTick(ctx context.Context, lease Lease, now time.Time, tickErr error) error {
	errorCode := any(nil)
	lastSuccess := any(nil)
	if tickErr == nil {
		lastSuccess = ISO(now)
	} else {
		errorCode = ErrorCode(tickErr)
	}
	result, err := s.db.ExecContext(ctx, `
    UPDATE membership_processor_lease
    SET status = 'active', last_tick_at = ?, last_success_at = COALESCE(?, last_success_at),
        last_error_code = ?, updated_at = ?
    WHERE id = 'default' AND owner = ? AND holder_token = ? AND epoch = ? AND expires_at > ?`,
		ISO(now), lastSuccess, errorCode, ISO(now), lease.Owner, lease.Token, lease.Epoch, ISO(now))
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrLeaseLost
	}
	return nil
}

func (s *Store) ReleaseLease(ctx context.Context, lease Lease, now time.Time) error {
	result, err := s.db.ExecContext(ctx, `
    UPDATE membership_processor_lease
    SET status = 'stopped', holder_token = NULL, expires_at = NULL, heartbeat_at = ?, updated_at = ?
    WHERE id = 'default' AND owner = ? AND holder_token = ? AND epoch = ?`,
		ISO(now), ISO(now), lease.Owner, lease.Token, lease.Epoch)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrLeaseLost
	}
	return nil
}

func ISO(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

type codedError interface{ Code() string }

func ErrorCode(err error) string {
	var coded codedError
	if errors.As(err, &coded) && coded.Code() != "" {
		return coded.Code()
	}
	return "MEMBERSHIP_PROCESSOR_FAILED"
}
