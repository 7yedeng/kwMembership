package provider

import (
	"context"
	"testing"
)

func TestSpaceXBalanceUsesSpendableFunds(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
		want float64
	}{
		{"reserve excluded", `{"balance":100,"spendable_balance":80,"account_reserve_amount":20,"account_reserve_enabled":true,"currency":"USD"}`, 80},
		{"total below reserve", `{"balance":10,"spendable_balance":0,"account_reserve_amount":20,"currency":"USD"}`, 0},
		{"negative ledger with explicit zero spendable", `{"balance":-1,"spendable_balance":0,"account_reserve_amount":20,"currency":"USD"}`, 0},
		{"subaccount no own reserve", `{"balance":100,"spendable_balance":100,"account_reserve_amount":0,"currency":"USD"}`, 100},
		{"provider value not local subtraction", `{"balance":100,"spendable_balance":75,"account_reserve_amount":20,"currency":"USD"}`, 75},
		{"spendable without reserve metadata", `{"balance":100,"spendable_balance":80,"currency":"USD"}`, 80},
		{"legacy total", `{"balance":100,"currency":"USD"}`, 100},
		{"legacy zero", `{"balance":0,"currency":"USD"}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newSpaceXResponseClient(t, `{"code":0,"data":`+tc.data+`}`)
			balance, err := client.GetBalance(context.Background())
			if err != nil || balance.Balance != tc.want || balance.Currency != "USD" {
				t.Fatalf("normalized funding balance=%+v, want %v USD, err=%v", balance, tc.want, err)
			}
		})
	}
}

func TestSpaceXBalanceRejectsUnknownOrInvalidSpendableFunds(t *testing.T) {
	for name, data := range map[string]string{
		"negative ledger with positive spendable": `{"balance":-1,"spendable_balance":1,"account_reserve_amount":20,"currency":"USD"}`,
		"legacy negative balance":                 `{"balance":-1,"currency":"USD"}`,
		"negative spendable":                      `{"balance":100,"spendable_balance":-1,"currency":"USD"}`,
		"spendable exceeds total":                 `{"balance":100,"spendable_balance":101,"currency":"USD"}`,
		"reserve alone":                           `{"balance":100,"account_reserve_amount":20,"currency":"USD"}`,
		"zero reserve alone":                      `{"balance":100,"account_reserve_amount":0,"currency":"USD"}`,
		"enabled reserve alone":                   `{"balance":100,"account_reserve_enabled":true,"currency":"USD"}`,
		"disabled reserve alone":                  `{"balance":100,"account_reserve_enabled":false,"currency":"USD"}`,
		"null spendable":                          `{"balance":100,"spendable_balance":null,"currency":"USD"}`,
		"string spendable":                        `{"balance":100,"spendable_balance":"80","currency":"USD"}`,
		"overflow spendable":                      `{"balance":100,"spendable_balance":1e309,"currency":"USD"}`,
		"negative reserve":                        `{"balance":100,"spendable_balance":80,"account_reserve_amount":-20,"currency":"USD"}`,
		"null reserve":                            `{"balance":100,"spendable_balance":80,"account_reserve_amount":null,"currency":"USD"}`,
		"invalid reserve status":                  `{"balance":100,"spendable_balance":80,"account_reserve_enabled":"true","currency":"USD"}`,
		"missing total":                           `{"spendable_balance":80,"account_reserve_amount":20,"currency":"USD"}`,
		"null total":                              `{"balance":null,"spendable_balance":80,"account_reserve_amount":20,"currency":"USD"}`,
		"incorrect currency":                      `{"balance":100,"spendable_balance":80,"currency":"EUR"}`,
	} {
		t.Run(name, func(t *testing.T) {
			client := newSpaceXResponseClient(t, `{"code":0,"data":`+data+`}`)
			balance, err := client.GetBalance(context.Background())
			requireProviderErrorCode(t, err, "SPACEXCARD_CONTRACT_DRIFT")
			if balance != (Balance{}) {
				t.Fatalf("invalid contract exposed a usable funding balance: %+v", balance)
			}
		})
	}
}
