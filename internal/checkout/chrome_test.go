package checkout

import (
	"testing"

	"github.com/chromedp/cdproto/cdp"
)

func TestMergeFactsRecognizesAllowlistedCheckoutAcrossFrames(t *testing.T) {
	amount := 1150.0
	request := Request{
		Mode: ModePreflight, Stage: "plus", TargetTier: "plus",
		PriceContract: PriceContract{ID: "price-1", Version: 1, Tier: "plus", Currency: "PHP", MinAmount: 1100, MaxAmount: 1200},
	}
	page, err := mergeFacts([]frameFact{
		{
			FrameID: "top", Origin: "https://chatgpt.com", RouteTemplate: "/checkout/{id}",
			Plan: "plus", Country: "PH", Currency: "PHP", DisplayedAmount: &amount,
			Controls: map[string]string{"submit": "payment-submit"}, Fields: map[string]bool{},
		},
		{
			FrameID: "stripe", Origin: "https://js.stripe.com", Fields: map[string]bool{
				"cardNumber": true, "expiry": true, "cvc": true, "billingName": true,
				"billingCountry": true, "billingPostal": true,
			}, Controls: map[string]string{},
		},
	}, "https://chatgpt.com", "/checkout/{id}", request, "checkout")
	if err != nil {
		t.Fatal(err)
	}
	if page.StateID != "PAYMENT_FINAL_READY" || page.controlFrames["submit"] != cdp.FrameID("top") || len(page.fieldFrames) != 1 {
		t.Fatalf("unexpected checkout page: state=%s controls=%v fieldFrames=%d", page.StateID, page.controlFrames, len(page.fieldFrames))
	}
	if page.StructuralHash == "" {
		t.Fatal("missing structural hash")
	}
}

func TestMergeFactsRejectsAmbiguousPaymentControls(t *testing.T) {
	amount := 1150.0
	request := Request{Mode: ModePreflight, Stage: "plus", TargetTier: "plus", PriceContract: PriceContract{
		ID: "price-1", Version: 1, Tier: "plus", Currency: "PHP", MinAmount: 1100, MaxAmount: 1200,
	}}
	page, err := mergeFacts([]frameFact{{
		FrameID: "top", Origin: "https://chatgpt.com", RouteTemplate: "/checkout", Plan: "plus",
		Country: "PH", Currency: "PHP", DisplayedAmount: &amount,
		Fields:   map[string]bool{"cardNumber": true, "expiry": true, "cvc": true, "billingName": true, "billingCountry": true, "billingPostal": true},
		Controls: map[string]string{"progression": "payment-next", "submit": "payment-submit"},
	}}, "https://chatgpt.com", "/checkout", request, "checkout")
	if err != nil {
		t.Fatal(err)
	}
	if page.StateID != "UNKNOWN_PAYMENT_STATE" {
		t.Fatalf("state = %s, want UNKNOWN_PAYMENT_STATE", page.StateID)
	}
}
