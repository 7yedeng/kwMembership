package processor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"kwmembership/internal/provider"
	"kwmembership/internal/store"
)

const (
	circuitFailureWindow = 5 * time.Minute
	circuitProbeTimeout  = 5 * time.Minute
	circuitInitialOpen   = 15 * time.Minute
	circuitMaxOpen       = time.Hour
)

var immediateCircuitCodes = map[string]bool{
	"SPACEXCARD_AUTH_FAILED": true, "SPACEXCARD_ACCESS_DENIED": true,
	"SPACEXCARD_CONTRACT_DRIFT": true, "SPACEXCARD_RESPONSE_INVALID": true,
	"EFUNCARD_AUTH_FAILED": true, "EFUNCARD_ACCESS_DENIED": true,
	"EFUNCARD_CONTRACT_DRIFT": true, "EFUNCARD_RESPONSE_INVALID": true,
	"MEMBERSHIP_CONTRACT_UNKNOWN": true, "MEMBERSHIP_PROVIDER_RESPONSE_INVALID": true,
	"MEMBERSHIP_PROVIDER_RESPONSE_TOO_LARGE": true,
}

var transientCircuitCodes = map[string]bool{
	"SPACEXCARD_RATE_LIMITED": true, "SPACEXCARD_TIMEOUT": true, "SPACEXCARD_UNAVAILABLE": true,
	"SPACEXCARD_RESPONSE_TOO_LARGE": true, "MEMBERSHIP_PROVIDER_RATE_LIMITED": true,
	"EFUNCARD_RATE_LIMITED": true, "EFUNCARD_TIMEOUT": true, "EFUNCARD_UNAVAILABLE": true,
	"EFUNCARD_RESPONSE_TOO_LARGE": true, "EFUNCARD_OPERATION_PENDING": true,
	"MEMBERSHIP_PROVIDER_TIMEOUT": true, "MEMBERSHIP_PROVIDER_UNAVAILABLE": true,
}

type cardPlatformConfig struct {
	Key                 string
	Kind                string
	BaseURL             sql.NullString
	CredentialEncrypted sql.NullString
}

func (p *Processor) hasCardPlatformTable(ctx context.Context) (bool, error) {
	var count int
	err := p.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master
    WHERE type='table' AND name='membership_card_platforms'`).Scan(&count)
	return count == 1, err
}

func (p *Processor) cardPlatform(ctx context.Context, key string) (provider.CardPlatform, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		key = provider.CardPlatformSpaceX
	}
	hasTable, err := p.hasCardPlatformTable(ctx)
	if err != nil {
		return nil, err
	}
	if hasTable {
		var config cardPlatformConfig
		err = p.store.DB().QueryRowContext(ctx, `SELECT key,kind,base_url,credential_encrypted
      FROM membership_card_platforms WHERE key=?`, key).
			Scan(&config.Key, &config.Kind, &config.BaseURL, &config.CredentialEncrypted)
		if err == nil && config.CredentialEncrypted.Valid && config.CredentialEncrypted.String != "" {
			plain, decryptErr := p.decrypter.Decrypt(config.CredentialEncrypted.String)
			if decryptErr != nil {
				return nil, codedWrap("CARD_PLATFORM_NOT_CONFIGURED", "decrypt card platform credential", decryptErr)
			}
			switch config.Kind {
			case provider.CardPlatformSpaceX:
				var credential struct {
					AppID     string `json:"appId"`
					AppSecret string `json:"appSecret"`
				}
				if json.Unmarshal([]byte(plain), &credential) != nil || strings.TrimSpace(credential.AppSecret) == "" {
					return nil, coded("CARD_PLATFORM_NOT_CONFIGURED", "SpaceX Card credential is invalid")
				}
				return provider.NewSpaceXClient(p.httpClient, credential.AppID, credential.AppSecret)
			case provider.CardPlatformEfun:
				var credential struct {
					APIKey string `json:"apiKey"`
				}
				if json.Unmarshal([]byte(plain), &credential) != nil || strings.TrimSpace(credential.APIKey) == "" {
					return nil, coded("CARD_PLATFORM_NOT_CONFIGURED", "EfunCard credential is invalid")
				}
				return provider.NewEfunCardClient(p.httpClient, config.BaseURL.String, credential.APIKey)
			default:
				return nil, coded("CARD_PLATFORM_KIND_UNSUPPORTED", "card platform kind is unsupported")
			}
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if key != provider.CardPlatformSpaceX {
			return nil, coded("CARD_PLATFORM_NOT_CONFIGURED", "card platform is not configured")
		}
	}
	if key != provider.CardPlatformSpaceX {
		return nil, coded("CARD_PLATFORM_NOT_CONFIGURED", "card platform is not configured")
	}
	return p.spaceXClient(ctx)
}

func (p *Processor) enabledCardPlatformKeys(ctx context.Context) ([]string, error) {
	hasTable, err := p.hasCardPlatformTable(ctx)
	if err != nil {
		return nil, err
	}
	if !hasTable {
		if _, err := p.spaceXClient(ctx); err != nil {
			return nil, err
		}
		return []string{provider.CardPlatformSpaceX}, nil
	}
	rows, err := p.store.DB().QueryContext(ctx, `SELECT key FROM membership_card_platforms
	    WHERE enabled=1 AND inventory_status='completed' ORDER BY priority,key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

func cardPlatformCircuit(key string) (string, string) {
	if key == provider.CardPlatformSpaceX {
		return "spacexcard_openapi", "default"
	}
	return "card_platform_openapi", key
}

func (p *Processor) spaceXClient(ctx context.Context) (*provider.SpaceXClient, error) {
	var appID, encrypted sql.NullString
	if err := p.store.DB().QueryRowContext(ctx, `SELECT spacexcard_app_id, spacexcard_app_secret_encrypted
    FROM membership_fulfillment_settings WHERE id='default'`).Scan(&appID, &encrypted); err != nil {
		return nil, err
	}
	if !encrypted.Valid || encrypted.String == "" {
		return nil, coded("SPACEXCARD_OPENAPI_NOT_CONFIGURED", "SpaceX Card OpenAPI is not configured")
	}
	secret, err := p.decrypter.Decrypt(encrypted.String)
	if err != nil {
		return nil, codedWrap("SPACEXCARD_OPENAPI_NOT_CONFIGURED", "decrypt SpaceX Card credential", err)
	}
	return provider.NewSpaceXClient(p.httpClient, appID.String, secret)
}

func (p *Processor) renewalToken(ctx context.Context) (string, error) {
	var encrypted sql.NullString
	if err := p.store.DB().QueryRowContext(ctx, `SELECT spacexcard_api_token_encrypted
    FROM extension_delivery_settings WHERE id='default'`).Scan(&encrypted); err != nil {
		return "", err
	}
	if !encrypted.Valid || encrypted.String == "" {
		return "", coded("RENEWAL_CANCEL_NOT_CONFIGURED", "renewal token is not configured")
	}
	value, err := p.decrypter.Decrypt(encrypted.String)
	if err != nil {
		return "", codedWrap("RENEWAL_CANCEL_NOT_CONFIGURED", "decrypt renewal token", err)
	}
	return value, nil
}

func (p *Processor) acquireCircuit(ctx context.Context, dependency, scope string, now time.Time) (bool, error) {
	allowed := false
	err := p.withFencedImmediate(ctx, func(tx *sql.Tx) error {
		var id, state, updatedAt string
		var retryAt sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT id,state,retry_at,updated_at FROM fulfillment_dependency_circuits
		  WHERE dependency=? AND scope_key=?`, dependency, scope).Scan(&id, &state, &retryAt, &updatedAt)
		if errors.Is(err, sql.ErrNoRows) {
			allowed = true
			return nil
		}
		if err != nil {
			return err
		}
		if state == "closed" {
			allowed = true
			return nil
		}
		if state == "half_open" {
			probeAt, err := time.Parse(time.RFC3339Nano, updatedAt)
			if err != nil || now.Sub(probeAt) < circuitProbeTimeout {
				return nil
			}
			result, err := tx.ExecContext(ctx, `UPDATE fulfillment_dependency_circuits SET updated_at=?
			  WHERE id=? AND state='half_open' AND updated_at=?`, store.ISO(now), id, updatedAt)
			if err != nil {
				return err
			}
			count, err := result.RowsAffected()
			if err != nil {
				return err
			}
			allowed = count == 1
			return nil
		}
		if state != "open" || !retryAt.Valid || retryAt.String > store.ISO(now) {
			return nil
		}
		result, err := tx.ExecContext(ctx, `UPDATE fulfillment_dependency_circuits SET state='half_open',updated_at=?
      WHERE id=? AND state='open' AND retry_at<=?`, store.ISO(now), id, store.ISO(now))
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		allowed = count == 1
		return nil
	})
	return allowed, err
}

func withCircuitAccounting(primary, accounting error) error {
	if accounting == nil {
		return primary
	}
	if primary == nil {
		return codedWrap("DEPENDENCY_CIRCUIT_ACCOUNTING_FAILED", "persist dependency circuit state", accounting)
	}
	// Keep the primary business/provider error first so health projections retain its code.
	return errors.Join(primary, fmt.Errorf("persist dependency circuit state: %w", accounting))
}

func (p *Processor) recordCircuitSuccess(ctx context.Context, dependency, scope string, now time.Time) error {
	_, err := p.fencedExec(ctx, `UPDATE fulfillment_dependency_circuits
    SET state='closed',failure_count=0,opened_at=NULL,retry_at=NULL,recovery_revision=0,
        reason_code=NULL,updated_at=? WHERE dependency=? AND scope_key=?`, store.ISO(now), dependency, scope)
	return err
}

func (p *Processor) recordCircuitFailure(ctx context.Context, dependency, scope string, cause error, now time.Time) error {
	code := errorCode(cause)
	if !immediateCircuitCodes[code] && !transientCircuitCodes[code] {
		return nil
	}
	openedNow := false
	return p.withFencedImmediate(ctx, func(tx *sql.Tx) error {
		var id, state, updatedAt string
		var failureCount, revision int
		err := tx.QueryRowContext(ctx, `SELECT id,state,failure_count,recovery_revision,updated_at
      FROM fulfillment_dependency_circuits WHERE dependency=? AND scope_key=?`, dependency, scope).
			Scan(&id, &state, &failureCount, &revision, &updatedAt)
		exists := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if !exists {
			id, state, failureCount, revision, updatedAt = "", "closed", 0, 0, ""
		}
		recent := false
		if parsed, err := time.Parse(time.RFC3339Nano, updatedAt); err == nil {
			recent = now.Sub(parsed) <= circuitFailureWindow
		}
		if state == "half_open" {
			failureCount = max(3, failureCount+1)
			revision++
		} else if recent {
			failureCount++
		} else {
			failureCount = 1
		}
		open := immediateCircuitCodes[code] || state == "half_open" || failureCount >= 3
		openedNow = open && state != "open"
		newState := "closed"
		var openedAt, retryAt any
		if open {
			newState, openedAt = "open", store.ISO(now)
			duration := circuitInitialOpen * time.Duration(1<<min(revision, 2))
			if duration > circuitMaxOpen {
				duration = circuitMaxOpen
			}
			retryAt = store.ISO(now.Add(duration))
		}
		if id == "" {
			var err error
			id, err = store.NewID("fdc_")
			if err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO fulfillment_dependency_circuits
      (id,dependency,scope_key,state,failure_count,opened_at,retry_at,recovery_revision,reason_code,updated_at)
      VALUES (?,?,?,?,?,?,?,?,?,?) ON CONFLICT(dependency,scope_key) DO UPDATE SET
        state=excluded.state,failure_count=excluded.failure_count,opened_at=excluded.opened_at,
        retry_at=excluded.retry_at,recovery_revision=excluded.recovery_revision,
        reason_code=excluded.reason_code,updated_at=excluded.updated_at`, id, dependency, scope, newState,
			failureCount, openedAt, retryAt, revision, code, store.ISO(now))
		if err != nil {
			return err
		}
		if openedNow {
			outboxID, err := store.NewID("mo_")
			if err != nil {
				return err
			}
			payload := `{"dependency":"` + dependency + `","scopeKey":"` + scope + `","reasonCode":"` + code + `"}`
			_, err = tx.ExecContext(ctx, `INSERT INTO membership_outbox
        (id,event_type,fulfillment_id,state_revision,payload,created_at)
        VALUES (?,'dependency.circuit.opened',NULL,NULL,?,?)`, outboxID, payload, store.ISO(now))
			return err
		}
		return nil
	})
}

func min(left, right int) int {
	if left < right {
		return left
	}
	return right
}
