package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSpaceXCardMaterialKeepsCardBillingFacts(t *testing.T) {
	cases := []struct {
		name       string
		fields     map[string]any
		holder     Cardholder
		holderName string
		billing    *BillingAddress
	}{
		{
			name: "complete address prefers returned full name",
			fields: map[string]any{
				"user_name": "Registered Holder", "first_name": "Given", "last_name": "Family",
				"card_address": map[string]any{
					"address_line_one": "9 Example Lane", "address_line_two": "Suite 2",
					"city": "London", "state": "", "country": "GB", "post_code": "SW1A 1AA",
				},
			},
			holder: Cardholder{FirstName: "Given", LastName: "Family"}, holderName: "Registered Holder",
			billing: &BillingAddress{Name: "Registered Holder", Line1: "9 Example Lane", Line2: "Suite 2", City: "London", Country: "GB", PostalCode: "SW1A 1AA"},
		},
		{
			name: "missing address still keeps joined holder name",
			fields: map[string]any{
				"first_name": "Given", "last_name": "Family",
			},
			holder: Cardholder{FirstName: "Given", LastName: "Family"}, holderName: "Given Family",
		},
		{
			name: "null address still keeps full name",
			fields: map[string]any{
				"user_name": "Registered Holder", "card_address": nil,
			},
			holderName: "Registered Holder",
		},
		{
			name: "empty object still keeps single first name",
			fields: map[string]any{
				"first_name": "Given", "card_address": map[string]any{},
			},
			holder: Cardholder{FirstName: "Given"}, holderName: "Given",
		},
		{
			name: "six empty fields do not invent an address",
			fields: map[string]any{
				"last_name": "Family",
				"card_address": map[string]any{
					"address_line_one": "", "address_line_two": "", "city": "", "state": "", "country": "", "post_code": "",
				},
			},
			holder: Cardholder{LastName: "Family"}, holderName: "Family",
		},
		{
			name: "partial address retains unknown country postal and holder",
			fields: map[string]any{
				"card_address": map[string]any{"city": "Paris"},
			},
			billing: &BillingAddress{City: "Paris"},
		},
		{
			name: "second line alone is still a partial address",
			fields: map[string]any{
				"first_name": "Given", "last_name": "Family",
				"card_address": map[string]any{"address_line_two": "Unit 2"},
			},
			holder: Cardholder{FirstName: "Given", LastName: "Family"}, holderName: "Given Family",
			billing: &BillingAddress{Name: "Given Family", Line2: "Unit 2"},
		},
		{
			name: "controls remain visible to downstream validation",
			fields: map[string]any{
				"user_name": "Returned Holder\n", "first_name": "Given\t", "last_name": "Family",
				"card_address": map[string]any{"address_line_one": "Example Lane\r", "country": "gb"},
			},
			holder: Cardholder{FirstName: "Given\t", LastName: "Family"}, holderName: "Returned Holder\n",
			billing: &BillingAddress{Name: "Returned Holder\n", Line1: "Example Lane\r", Country: "gb"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := cardBillingFixtureClient(t, func(request *http.Request) map[string]any {
				calls++
				if request.URL.Path != "/openapi/v1/cards/7" {
					t.Fatal("billing was requested outside the selected card detail")
				}
				return tc.fields
			})
			material, err := client.GetCardMaterial(context.Background(), 7, time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC))
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("card detail requests = %d, want 1", calls)
			}
			if material.Holder != tc.holder || material.HolderName != tc.holderName || !reflect.DeepEqual(material.Billing, tc.billing) {
				t.Fatal("card holder or billing facts were lost, substituted, or invented")
			}
			if material.Number != "4242424242424242" || material.CVV != "123" || material.ExpiryMonth != "08" || material.ExpiryYear != "2029" || material.Status != "ACTIVE" {
				t.Fatal("existing card material fields changed")
			}
		})
	}
}

func TestSpaceXCardBillingIsBoundToEachRequestedCard(t *testing.T) {
	var paths []string
	client := cardBillingFixtureClient(t, func(request *http.Request) map[string]any {
		paths = append(paths, request.URL.Path)
		switch request.URL.Path {
		case "/openapi/v1/cards/7":
			return map[string]any{"user_name": "First Holder", "card_address": map[string]any{"address_line_one": "First Card Address"}}
		case "/openapi/v1/cards/8":
			return map[string]any{"user_name": "Second Holder", "card_address": map[string]any{"address_line_one": "Second Card Address"}}
		default:
			t.Fatal("unexpected request beyond the selected card details")
			return nil
		}
	})
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	first, err := client.GetCardMaterial(context.Background(), 7, now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.GetCardMaterial(context.Background(), 8, now)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(paths, []string{"/openapi/v1/cards/7", "/openapi/v1/cards/8"}) {
		t.Fatal("card material lookup made an unrelated or repeated request")
	}
	if first.Billing == nil || second.Billing == nil || first.Billing == second.Billing ||
		first.Billing.Name != "First Holder" || first.Billing.Line1 != "First Card Address" ||
		second.Billing.Name != "Second Holder" || second.Billing.Line1 != "Second Card Address" {
		t.Fatal("billing was not kept with its requested card")
	}
}

func TestSpaceXCardBillingMalformedResponseDoesNotEchoSensitiveFields(t *testing.T) {
	client := cardBillingFixtureClient(t, func(*http.Request) map[string]any {
		return map[string]any{"user_name": "PRIVATE-HOLDER", "card_address": "PRIVATE-ADDRESS"}
	})
	material, err := client.GetCardMaterial(context.Background(), 7, time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC))
	requireProviderErrorCode(t, err, "SPACEXCARD_CONTRACT_DRIFT")
	if material != (CardMaterial{}) || strings.Contains(err.Error(), "PRIVATE-") || strings.Contains(err.Error(), "4242424242424242") {
		t.Fatal("malformed card response leaked material or sensitive text")
	}
}

type cardBillingTransport func(*http.Request) (*http.Response, error)

func (transport cardBillingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

// This transport returns synthetic fixtures directly; no listener or external
// network connection is used by the card billing tests.
func cardBillingFixtureClient(t *testing.T, fields func(*http.Request) map[string]any) *SpaceXClient {
	t.Helper()
	client, err := NewSpaceXClientWithBaseURL(&http.Client{Transport: cardBillingTransport(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.Host != "cards.example" || request.URL.RawQuery != "" {
			t.Fatal("unexpected card billing request")
		}
		data := map[string]any{"card_number": "4242424242424242", "cvv": "123", "expire": "08/29", "status": "ACTIVE"}
		for key, value := range fields(request) {
			data[key] = value
		}
		encoded, err := json.Marshal(map[string]any{"code": 0, "data": data})
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(encoded)), Request: request}, nil
	})}, "https://cards.example", "fixture-app", "fixture-key")
	if err != nil {
		t.Fatal(err)
	}
	return client
}
