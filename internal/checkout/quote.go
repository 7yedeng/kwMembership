package checkout

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strconv"
	"strings"
)

const MaxQuoteInputBytes = 16 << 10

// QuoteTarget names the intended product and explicit checkout market.
// This metadata does not extend the executor's supported plans or markets.
type QuoteTarget struct {
	Plan     string `json:"plan"`
	Country  string `json:"country"`
	Currency string `json:"currency"`
}

// QuoteEvidence contains locally supplied price facts. Every amount, including
// zero tax/discount, must be explicit. TaxIncluded describes the subtotal:
// false => subtotal - discount + tax; true => subtotal - discount.
// In the inclusive case Tax is the included tax component of the final total.
type QuoteEvidence struct {
	Plan        string  `json:"plan"`
	Country     string  `json:"country"`
	Currency    string  `json:"currency"`
	Subtotal    *string `json:"subtotal"`
	Tax         *string `json:"tax"`
	Discount    *string `json:"discount"`
	Total       *string `json:"total"`
	TaxIncluded *bool   `json:"taxIncluded"`
}

type QuoteBudget struct {
	Min string `json:"min"`
	Max string `json:"max"`
}

type QuoteReviewRequest struct {
	Expected QuoteTarget   `json:"expected"`
	Evidence QuoteEvidence `json:"evidence"`
	Budget   QuoteBudget   `json:"budget"`
}

// ReviewedQuote only contains validated enums and integer minor units. It
// contains no account, card, credential, free-form text or checkout URL.
type ReviewedQuote struct {
	Plan           string `json:"plan"`
	Country        string `json:"country"`
	Currency       string `json:"currency"`
	MinorUnitScale int    `json:"minorUnitScale"`
	SubtotalMinor  int64  `json:"subtotalMinor"`
	TaxMinor       int64  `json:"taxMinor"`
	DiscountMinor  int64  `json:"discountMinor"`
	TotalMinor     int64  `json:"totalMinor"`
	TaxIncluded    bool   `json:"taxIncluded"`
	BudgetMinMinor int64  `json:"budgetMinMinor"`
	BudgetMaxMinor int64  `json:"budgetMaxMinor"`
}

type QuoteReviewReport struct {
	Operation      string         `json:"operation"`
	Code           string         `json:"code"`
	EvidenceSource string         `json:"evidenceSource"`
	ReviewReady    bool           `json:"reviewReady"`
	PaymentAllowed bool           `json:"paymentAllowed"`
	RemoteVerified bool           `json:"remoteVerified"`
	Quote          *ReviewedQuote `json:"quote,omitempty"`
}

type quoteError struct{ code string }

func (e *quoteError) Error() string { return e.code }
func (e *quoteError) Code() string  { return e.code }

var quoteCodes = map[string]bool{
	"QUOTE_FAILED": true, "QUOTE_INPUT_INVALID": true, "QUOTE_INPUT_TOO_LARGE": true,
	"QUOTE_ARGUMENTS_INVALID": true, "QUOTE_CANCELLED": true, "QUOTE_TIMEOUT": true,
	"QUOTE_TARGET_UNSUPPORTED": true, "QUOTE_TARGET_MISMATCH": true,
	"QUOTE_INCOMPLETE": true, "QUOTE_AMOUNT_INVALID": true, "QUOTE_AMOUNT_OVERFLOW": true,
	"QUOTE_TOTAL_MISMATCH": true, "QUOTE_BUDGET_INVALID": true, "QUOTE_OUTSIDE_BUDGET": true,
}

func quoteFail(code string) error { return &quoteError{code: code} }

func SafeQuoteCode(err error) string {
	var coded interface{ Code() string }
	if errors.As(err, &coded) && quoteCodes[coded.Code()] {
		return coded.Code()
	}
	return "QUOTE_FAILED"
}

func QuoteFailure(code string) QuoteReviewReport {
	if !quoteCodes[code] {
		code = "QUOTE_FAILED"
	}
	return QuoteReviewReport{
		Operation: "review", Code: code, EvidenceSource: "local_input",
		ReviewReady: false, PaymentAllowed: false, RemoteVerified: false,
	}
}

// ParseQuoteMinor accepts an unsigned base-10 string with at most two decimal
// places. No float is involved, including during JSON decoding.
func ParseQuoteMinor(value, currency string) (int64, error) {
	if currency != "PHP" && currency != "EGP" {
		return 0, quoteFail("QUOTE_TARGET_UNSUPPORTED")
	}
	if value == "" || len(value) > 32 {
		return 0, quoteFail("QUOTE_AMOUNT_INVALID")
	}
	parts := strings.Split(value, ".")
	if len(parts) > 2 || parts[0] == "" || (len(parts[0]) > 1 && parts[0][0] == '0') {
		return 0, quoteFail("QUOTE_AMOUNT_INVALID")
	}
	fraction := ""
	if len(parts) == 2 {
		fraction = parts[1]
		if len(fraction) < 1 || len(fraction) > 2 {
			return 0, quoteFail("QUOTE_AMOUNT_INVALID")
		}
	}
	for _, part := range parts {
		for _, char := range part {
			if char < '0' || char > '9' {
				return 0, quoteFail("QUOTE_AMOUNT_INVALID")
			}
		}
	}
	for len(fraction) < 2 {
		fraction += "0"
	}
	minor, err := strconv.ParseInt(parts[0]+fraction, 10, 64)
	if err != nil {
		return 0, quoteFail("QUOTE_AMOUNT_OVERFLOW")
	}
	return minor, nil
}

func supportedQuoteTarget(target QuoteTarget) bool {
	if target.Plan != "plus" && target.Plan != "go" && target.Plan != "x5" && target.Plan != "x20" {
		return false
	}
	return target.Country == "PH" && target.Currency == "PHP" ||
		target.Country == "EG" && target.Currency == "EGP"
}

// ReviewQuote checks arithmetic and a local budget only. A ready report is
// never a payment permit, remote observation, identity proof or executable task.
func ReviewQuote(request QuoteReviewRequest) QuoteReviewReport {
	if !supportedQuoteTarget(request.Expected) {
		return QuoteFailure("QUOTE_TARGET_UNSUPPORTED")
	}
	evidence := request.Evidence
	observed := QuoteTarget{Plan: evidence.Plan, Country: evidence.Country, Currency: evidence.Currency}
	if observed != request.Expected {
		return QuoteFailure("QUOTE_TARGET_MISMATCH")
	}
	if evidence.Subtotal == nil || evidence.Tax == nil || evidence.Discount == nil ||
		evidence.Total == nil || evidence.TaxIncluded == nil {
		return QuoteFailure("QUOTE_INCOMPLETE")
	}
	values := []*string{evidence.Subtotal, evidence.Tax, evidence.Discount, evidence.Total}
	amounts := make([]int64, 4)
	for index, value := range values {
		amount, err := ParseQuoteMinor(*value, evidence.Currency)
		if err != nil {
			return QuoteFailure(SafeQuoteCode(err))
		}
		amounts[index] = amount
	}
	subtotal, tax, discount, total := amounts[0], amounts[1], amounts[2], amounts[3]
	if discount > subtotal {
		return QuoteFailure("QUOTE_TOTAL_MISMATCH")
	}
	expectedTotal := subtotal - discount
	if *evidence.TaxIncluded {
		if tax > expectedTotal {
			return QuoteFailure("QUOTE_TOTAL_MISMATCH")
		}
	} else {
		if tax > math.MaxInt64-expectedTotal {
			return QuoteFailure("QUOTE_AMOUNT_OVERFLOW")
		}
		expectedTotal += tax
	}
	if expectedTotal != total {
		return QuoteFailure("QUOTE_TOTAL_MISMATCH")
	}
	min, errMin := ParseQuoteMinor(request.Budget.Min, evidence.Currency)
	max, errMax := ParseQuoteMinor(request.Budget.Max, evidence.Currency)
	if errMin != nil || errMax != nil || min > max {
		return QuoteFailure("QUOTE_BUDGET_INVALID")
	}
	if total < min || total > max {
		return QuoteFailure("QUOTE_OUTSIDE_BUDGET")
	}
	return QuoteReviewReport{
		Operation: "review", Code: "QUOTE_REVIEW_READY", EvidenceSource: "local_input",
		ReviewReady: true, PaymentAllowed: false, RemoteVerified: false,
		Quote: &ReviewedQuote{
			Plan: evidence.Plan, Country: evidence.Country, Currency: evidence.Currency,
			MinorUnitScale: 2, SubtotalMinor: subtotal, TaxMinor: tax,
			DiscountMinor: discount, TotalMinor: total, TaxIncluded: *evidence.TaxIncluded,
			BudgetMinMinor: min, BudgetMaxMinor: max,
		},
	}
}

// rejectDuplicateQuoteKeys rejects repeated keys, case aliases and objects
// nested beyond this contract's depth before the typed decoder runs.
func rejectDuplicateQuoteKeys(raw []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value func(int) bool
	value = func(depth int) bool {
		if depth > 4 {
			return false
		}
		token, err := decoder.Token()
		if err != nil {
			return false
		}
		delimiter, compound := token.(json.Delim)
		if !compound {
			return true
		}
		if delimiter != '{' {
			return false
		}
		seen := make(map[string]bool)
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return false
			}
			key, ok := keyToken.(string)
			if !ok {
				return false
			}
			normalized := strings.ToLower(key)
			if seen[normalized] {
				return false
			}
			seen[normalized] = true
			if !value(depth + 1) {
				return false
			}
		}
		end, err := decoder.Token()
		return err == nil && end == json.Delim('}')
	}
	if !value(0) {
		return false
	}
	_, err := decoder.Token()
	return err == io.EOF
}

// DecodeQuoteReview rejects unknown fields, numbers used as money, duplicate
// object keys, trailing documents and overlarge inputs. Errors never echo input.
func DecodeQuoteReview(reader io.Reader) (QuoteReviewRequest, error) {
	if reader == nil {
		return QuoteReviewRequest{}, quoteFail("QUOTE_INPUT_INVALID")
	}
	raw, err := io.ReadAll(io.LimitReader(reader, MaxQuoteInputBytes+1))
	if err != nil {
		return QuoteReviewRequest{}, quoteFail("QUOTE_INPUT_INVALID")
	}
	if len(raw) > MaxQuoteInputBytes {
		return QuoteReviewRequest{}, quoteFail("QUOTE_INPUT_TOO_LARGE")
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' || !rejectDuplicateQuoteKeys(raw) {
		return QuoteReviewRequest{}, quoteFail("QUOTE_INPUT_INVALID")
	}
	var request QuoteReviewRequest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil {
		return QuoteReviewRequest{}, quoteFail("QUOTE_INPUT_INVALID")
	}
	return request, nil
}
