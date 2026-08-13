package processor

import (
	"context"
	"fmt"

	"kwmembership/internal/store"
)

func tableHasColumn(ctx context.Context, query store.Execer, table, column string) (bool, error) {
	allowed := map[string]bool{
		"managed_cards": true, "card_capacity_reservations": true,
		"funding_intents": true, "card_product_policies": true,
	}
	if !allowed[table] {
		return false, fmt.Errorf("unsupported schema compatibility table %q", table)
	}
	rows, err := query.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var sequence, notNull, primaryKey int
		var name, dataType string
		var defaultValue any
		if err := rows.Scan(&sequence, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}
