package checkout

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
)

func quoteFixture() QuoteReviewRequest {
	subtotal, tax, discount, total := "200.00", "28.00", "0.00", "228.00"
	included := false
	return QuoteReviewRequest{
		Expected: QuoteTarget{Plan: "go", Country: "EG", Currency: "EGP"},
		Evidence: QuoteEvidence{Plan: "go", Country: "EG", Currency: "EGP",
			Subtotal: &subtotal, Tax: &tax, Discount: &discount, Total: &total, TaxIncluded: &included},
		Budget: QuoteBudget{Min: "220.00", Max: "230.00"},
	}
}
func quoteValue(value string) *string { return &value }
func quoteJSON(t *testing.T, value QuoteReviewRequest) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestQuoteMoneyUsesExactMinorUnitsAndRejectsAmbiguousInputs(t *testing.T) {
	for _, fixture := range []struct {
		input string
		want  int64
	}{
		{"0", 0}, {"0.01", 1}, {"1.1", 110}, {"982.14", 98214},
		{"92233720368547758.07", math.MaxInt64},
	} {
		for _, currency := range []string{"PHP", "EGP"} {
			got, err := ParseQuoteMinor(fixture.input, currency)
			if err != nil || got != fixture.want {
				t.Fatalf("input=%s, got=%d, err=%v", fixture.input, got, err)
			}
		}
	}
	for _, input := range []string{
		"", "-1", "-0", "+1", "1e3", "NaN", "Infinity", "1,000.00", "1.001",
		"1.", ".1", "01.00", " 1.00", "1.00 ", "1\n", "１２.００",
		"92233720368547758.08", "999999999999999999999999999",
	} {
		if _, err := ParseQuoteMinor(input, "EGP"); err == nil {
			t.Fatalf("accepted invalid money: %q", input)
		}
	}
	if _, err := ParseQuoteMinor("1.00", "USD"); SafeQuoteCode(err) != "QUOTE_TARGET_UNSUPPORTED" {
		t.Fatal(err)
	}
}

func TestQuoteExclusivePriceCompletesLocalReviewOnly(t *testing.T) {
	request := quoteFixture()
	decoded, err := DecodeQuoteReview(strings.NewReader(quoteJSON(t, request)))
	if err != nil {
		t.Fatal(err)
	}
	report := ReviewQuote(decoded)
	if !report.ReviewReady || report.Code != "QUOTE_REVIEW_READY" ||
		report.PaymentAllowed || report.RemoteVerified || report.EvidenceSource != "local_input" {
		t.Fatalf("unexpected report: %+v", report)
	}
	if report.Quote.TotalMinor != 22800 || report.Quote.SubtotalMinor != 20000 ||
		report.Quote.TaxMinor != 2800 || report.Quote.DiscountMinor != 0 || report.Quote.MinorUnitScale != 2 {
		t.Fatalf("wrong normalized amounts: %+v", report.Quote)
	}
	// A PH/PHP quote uses the same decimal conversion, without market inference.
	request.Expected = QuoteTarget{Plan: "plus", Country: "PH", Currency: "PHP"}
	request.Evidence.Plan, request.Evidence.Country, request.Evidence.Currency = "plus", "PH", "PHP"
	if report := ReviewQuote(request); !report.ReviewReady || report.Quote.Currency != "PHP" {
		t.Fatal(report)
	}
}

func TestQuoteTaxIncludedIsNotAddedTwice(t *testing.T) {
	request := quoteFixture()
	*request.Evidence.TaxIncluded = true
	request.Evidence.Subtotal, request.Evidence.Tax = quoteValue("110.00"), quoteValue("9.09")
	request.Evidence.Discount, request.Evidence.Total = quoteValue("10.00"), quoteValue("100.00")
	request.Budget = QuoteBudget{Min: "100.00", Max: "100.00"}
	if report := ReviewQuote(request); !report.ReviewReady || report.Quote.TotalMinor != 10000 {
		t.Fatal(report)
	}
	request.Evidence.Total = quoteValue("109.09")
	if report := ReviewQuote(request); report.Code != "QUOTE_TOTAL_MISMATCH" {
		t.Fatal(report)
	}
	request.Evidence.Total, request.Evidence.Tax = quoteValue("100.00"), quoteValue("100.01")
	if report := ReviewQuote(request); report.Code != "QUOTE_TOTAL_MISMATCH" {
		t.Fatal(report)
	}
}

func TestQuoteMissingEvidenceDoesNotBecomeZero(t *testing.T) {
	for _, missing := range []string{"subtotal", "tax", "discount", "total", "taxIncluded"} {
		t.Run(missing, func(t *testing.T) {
			request := quoteFixture()
			switch missing {
			case "subtotal":
				request.Evidence.Subtotal = nil
			case "tax":
				request.Evidence.Tax = nil
			case "discount":
				request.Evidence.Discount = nil
			case "total":
				request.Evidence.Total = nil
			case "taxIncluded":
				request.Evidence.TaxIncluded = nil
			}
			if report := ReviewQuote(request); report.Code != "QUOTE_INCOMPLETE" ||
				report.ReviewReady || report.Quote != nil || report.PaymentAllowed || report.RemoteVerified {
				t.Fatal(report)
			}
		})
	}
	request := quoteFixture()
	request.Evidence.Subtotal, request.Evidence.Tax = quoteValue("0"), quoteValue("0")
	request.Evidence.Discount, request.Evidence.Total = quoteValue("0"), quoteValue("0")
	request.Budget = QuoteBudget{Min: "0", Max: "0"}
	if report := ReviewQuote(request); !report.ReviewReady || report.Quote.TotalMinor != 0 {
		t.Fatal(report)
	}
}

func TestQuoteRejectsWrongTaxMathOverflowAndOverDiscount(t *testing.T) {
	for _, fixture := range []struct {
		name   string
		mutate func(*QuoteReviewRequest)
		code   string
	}{
		{"wrong tax", func(r *QuoteReviewRequest) { r.Evidence.Tax = quoteValue("27.99") }, "QUOTE_TOTAL_MISMATCH"},
		{"overflow sum", func(r *QuoteReviewRequest) {
			r.Evidence.Subtotal, r.Evidence.Tax = quoteValue("92233720368547758.07"), quoteValue("0.01")
			r.Evidence.Total = quoteValue("92233720368547758.07")
		}, "QUOTE_AMOUNT_OVERFLOW"},
		{"over discount", func(r *QuoteReviewRequest) { r.Evidence.Discount = quoteValue("200.01") }, "QUOTE_TOTAL_MISMATCH"},
		{"negative tax", func(r *QuoteReviewRequest) { r.Evidence.Tax = quoteValue("-1.00") }, "QUOTE_AMOUNT_INVALID"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			request := quoteFixture()
			fixture.mutate(&request)
			report := ReviewQuote(request)
			if report.Code != fixture.code || report.ReviewReady || report.Quote != nil {
				t.Fatal(report)
			}
		})
	}
}

func TestQuoteTargetMustMatchWithoutCurrencyOrPlanFallback(t *testing.T) {
	for _, change := range []func(*QuoteReviewRequest){
		func(r *QuoteReviewRequest) { r.Evidence.Currency = "PHP" },
		func(r *QuoteReviewRequest) { r.Evidence.Country = "PH" },
		func(r *QuoteReviewRequest) { r.Evidence.Plan = "plus" },
	} {
		request := quoteFixture()
		change(&request)
		if report := ReviewQuote(request); report.Code != "QUOTE_TARGET_MISMATCH" || report.Quote != nil {
			t.Fatal(report)
		}
	}
	for _, target := range []QuoteTarget{
		{Plan: "go", Country: "EG", Currency: "PHP"},
		{Plan: "go", Country: "", Currency: "PHP"},
		{Plan: "unknown-private-value", Country: "EG", Currency: "EGP"},
	} {
		request := quoteFixture()
		request.Expected = target
		if report := ReviewQuote(request); report.Code != "QUOTE_TARGET_UNSUPPORTED" {
			t.Fatal(report)
		}
	}
}

func TestQuoteBudgetChecksFinalTotalAtBothInclusiveBoundaries(t *testing.T) {
	for _, budget := range []QuoteBudget{{Min: "228.00", Max: "228.00"}, {Min: "220", Max: "228"}, {Min: "228", Max: "240"}} {
		request := quoteFixture()
		request.Budget = budget
		if report := ReviewQuote(request); !report.ReviewReady {
			t.Fatal(report)
		}
	}
	for _, fixture := range []struct {
		budget QuoteBudget
		code   string
	}{
		{QuoteBudget{Min: "228.01", Max: "230"}, "QUOTE_OUTSIDE_BUDGET"},
		{QuoteBudget{Min: "200", Max: "227.99"}, "QUOTE_OUTSIDE_BUDGET"},
		{QuoteBudget{Min: "240", Max: "200"}, "QUOTE_BUDGET_INVALID"},
		{QuoteBudget{Min: "", Max: "230"}, "QUOTE_BUDGET_INVALID"},
	} {
		request := quoteFixture()
		request.Budget = fixture.budget
		if report := ReviewQuote(request); report.Code != fixture.code || report.Quote != nil {
			t.Fatal(report)
		}
	}
}

func TestQuoteDecodeRejectsUnknownDuplicateFloatAndTrailingDocuments(t *testing.T) {
	raw := quoteJSON(t, quoteFixture())
	invalid := []string{
		raw[:len(raw)-1] + `,"session":"PRIVATE_SENTINEL"}`,
		strings.Replace(raw, `"tax":"28.00"`, `"tax":28.00`, 1),
		strings.Replace(raw, `"tax":"28.00"`, `"tax":"28.00","tax":"0.00"`, 1),
		strings.Replace(raw, `"tax":"28.00"`, `"tax":"28.00","Tax":"0.00"`, 1),
		strings.Replace(raw, `"tax":"28.00"`, `"tax":"28.00","card":"PRIVATE_SENTINEL"`, 1),
		raw + " {}", "null", "[]",
	}
	for _, input := range invalid {
		_, err := DecodeQuoteReview(strings.NewReader(input))
		if SafeQuoteCode(err) != "QUOTE_INPUT_INVALID" || strings.Contains(err.Error(), "PRIVATE") {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	_, err := DecodeQuoteReview(strings.NewReader(strings.Repeat(" ", MaxQuoteInputBytes+1)))
	if SafeQuoteCode(err) != "QUOTE_INPUT_TOO_LARGE" {
		t.Fatal(err)
	}
}

func TestQuoteReportsDoNotEchoUntrustedInput(t *testing.T) {
	request := quoteFixture()
	request.Evidence.Plan = "PRIVATE_SESSION_OR_CARD"
	report := ReviewQuote(request)
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "PRIVATE") || report.PaymentAllowed || report.RemoteVerified || report.Quote != nil {
		t.Fatal(string(raw))
	}
	if SafeQuoteCode(errors.New("PRIVATE_UPSTREAM")) != "QUOTE_FAILED" ||
		QuoteFailure("PRIVATE_CODE").Code != "QUOTE_FAILED" {
		t.Fatal("unsafe error code")
	}
}
