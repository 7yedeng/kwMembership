package preflight

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kwmembership/internal/checkout"
)

var testNow = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

func testInput(t *testing.T, mutate func(map[string]any)) string {
	t.Helper()
	item := map[string]any{
		"targetTier": "plus", "expectedEmail": "buyer@example.invalid",
		"session": map[string]any{
			"sessionToken": "PRIVATE-SESSION-TOKEN", "accessToken": "PRIVATE-ACCESS-TOKEN",
			"user":    map[string]any{"email": "buyer@example.invalid", "id": "PRIVATE-USER-ID"},
			"expires": "2099-01-01T00:00:00Z",
		},
		// Synthetic test range, not a statement about the current product price.
		"priceContract": map[string]any{"id": "test-contract", "version": 1, "tier": "plus", "currency": "PHP", "minAmount": 100, "maxAmount": 200},
	}
	if mutate != nil {
		mutate(item)
	}
	raw, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func preparedInput(t *testing.T) Prepared {
	t.Helper()
	prepared, err := Decode(strings.NewReader(testInput(t, nil)), testNow)
	if err != nil {
		t.Fatal(err)
	}
	return prepared
}

func TestCheckIsLocalAndNeverReportsRemoteVerification(t *testing.T) {
	report := Check(preparedInput(t))
	if !report.InputValid || report.RemoteVerified || report.PaymentAllowed || report.PreflightPassed || report.Page != nil || report.Code != "PREFLIGHT_INPUT_VALID" {
		t.Fatalf("local check overclaimed its result: %+v", report)
	}
	assertReportSanitized(t, report)
}

func TestDecodeRejectsInvalidOrPaymentShapedInput(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		mutate     func(map[string]any)
	}{
		{"unknown material", "PREFLIGHT_INPUT_INVALID", func(v map[string]any) { v["material"] = map[string]any{"number": "PRIVATE-CARD"} }},
		{"unknown contract field", "PREFLIGHT_INPUT_INVALID", func(v map[string]any) { v["priceContract"].(map[string]any)["approved"] = true }},
		{"unsupported target", "PREFLIGHT_TARGET_UNSUPPORTED", func(v map[string]any) { v["targetTier"] = "x20" }},
		{"unsupported contract", "PREFLIGHT_PRICE_CONTRACT_INVALID", func(v map[string]any) { v["priceContract"].(map[string]any)["tier"] = "x5" }},
		{"wrong currency", "PREFLIGHT_PRICE_CONTRACT_INVALID", func(v map[string]any) { v["priceContract"].(map[string]any)["currency"] = "USD" }},
		{"zero price", "PREFLIGHT_PRICE_CONTRACT_INVALID", func(v map[string]any) { v["priceContract"].(map[string]any)["minAmount"] = 0 }},
		{"reversed range", "PREFLIGHT_PRICE_CONTRACT_INVALID", func(v map[string]any) { v["priceContract"].(map[string]any)["maxAmount"] = 1 }},
		{"invalid version type", "PREFLIGHT_INPUT_INVALID", func(v map[string]any) { v["priceContract"].(map[string]any)["version"] = "1" }},
		{"missing identity", "EXPECTED_IDENTITY_MISSING", func(v map[string]any) { v["expectedEmail"] = "" }},
		{"identity mismatch", "CHATGPT_SESSION_IDENTITY_MISMATCH", func(v map[string]any) { v["expectedEmail"] = "another@example.invalid" }},
		{"access token only", "SESSION_COOKIE_MISSING", func(v map[string]any) { delete(v["session"].(map[string]any), "sessionToken") }},
		{"expired session", "CHATGPT_SESSION_UNAUTHORIZED", func(v map[string]any) { v["session"].(map[string]any)["expires"] = "2020-01-01T00:00:00Z" }},
		{"invalid session expiry", "SESSION_INVALID", func(v map[string]any) { v["session"].(map[string]any)["expires"] = "PRIVATE-BAD-EXPIRY" }},
		{"session payload limit", "PREFLIGHT_INPUT_TOO_LARGE", func(v map[string]any) { v["session"].(map[string]any)["extra"] = strings.Repeat("x", MaxSessionBytes) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Decode(strings.NewReader(testInput(t, tc.mutate)), testNow)
			if SafeCode(err) != tc.code {
				t.Fatalf("code=%s, want %s", SafeCode(err), tc.code)
			}
			if err == nil || strings.Contains(err.Error(), "PRIVATE") || strings.Contains(err.Error(), "@") {
				t.Fatal("invalid input accepted or echoed")
			}
		})
	}
	for name, raw := range map[string]string{
		"trailing JSON": testInput(t, nil) + ` {"secret":"PRIVATE-TAIL"}`,
		"trailing text": testInput(t, nil) + ` PRIVATE-TAIL`,
		"invalid JSON":  `{"session":"PRIVATE-TOKEN"`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(strings.NewReader(raw), testNow); SafeCode(err) != "PREFLIGHT_INPUT_INVALID" {
				t.Fatalf("malformed input accepted: %v", err)
			}
		})
	}
	if _, err := Decode(strings.NewReader(strings.Repeat(" ", MaxInputBytes+1)), testNow); SafeCode(err) != "PREFLIGHT_INPUT_TOO_LARGE" {
		t.Fatalf("input cap not enforced: %v", err)
	}
}

type executorFunc func(context.Context, checkout.Request) (checkout.Result, error)

func (f executorFunc) Execute(ctx context.Context, request checkout.Request) (checkout.Result, error) {
	return f(ctx, request)
}

func testPage() checkout.PageFacts {
	amount := 150.0
	return checkout.PageFacts{
		StateID: "PAYMENT_FINAL_READY", Origin: "https://chatgpt.com", RouteTemplate: "/checkout/{id}",
		Plan: "plus", Country: "PH", Currency: "PHP", DisplayedAmount: &amount,
		Controls: map[string]string{"submit": "PRIVATE-CONTROL"}, StructuralHash: "PRIVATE-HASH",
	}
}

func TestRunBuildsOnlySessionPreflightAndSanitizesResult(t *testing.T) {
	called := false
	executor := executorFunc(func(_ context.Context, request checkout.Request) (checkout.Result, error) {
		called = true
		if request.Mode != checkout.ModeSessionPreflight || request.TargetTier != "plus" || request.Stage != "plus" ||
			request.Material != nil || request.Guard != nil || request.OnHandoff != nil ||
			request.Binding != (checkout.ExecutionBinding{}) || request.CheckoutURL != "" || request.PlanURL != "" || len(request.Session) != 0 ||
			len(request.Cookies) == 0 || request.ExpectedEmail != "buyer@example.invalid" {
			t.Fatal("unsafe or incomplete standalone execution request")
		}
		return checkout.Result{Page: testPage()}, nil
	})
	report, err := Run(context.Background(), preparedInput(t), executor)
	if err != nil || !called || !report.RemoteVerified || !report.PreflightPassed || report.PaymentAllowed || report.Code != "PREFLIGHT_PASSED" {
		t.Fatalf("preflight result=%+v, err=%v", report, err)
	}
	assertReportSanitized(t, report)
}

func TestRunRefusesPaymentActionsChallengesAndUnknownEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		mutate     func(*checkout.Result)
	}{
		{"submit", "PREFLIGHT_BOUNDARY_VIOLATION", func(r *checkout.Result) { r.SubmitActivated = true }},
		{"progression", "PREFLIGHT_BOUNDARY_VIOLATION", func(r *checkout.Result) { r.ProgressionCount = 1 }},
		{"guard", "PREFLIGHT_BOUNDARY_VIOLATION", func(r *checkout.Result) { r.StoppedByGuard = true }},
		{"challenge", "SECURITY_CHALLENGE_REQUIRED", func(r *checkout.Result) { r.Challenge = true }},
		{"foreign origin", "CHECKOUT_UI_UNSUPPORTED", func(r *checkout.Result) { r.Page.Origin = "https://PRIVATE.invalid" }},
		{"raw route", "CHECKOUT_UI_UNSUPPORTED", func(r *checkout.Result) { r.Page.RouteTemplate = "/checkout/PRIVATE-TOKEN" }},
		{"wrong plan", "CHECKOUT_UI_UNSUPPORTED", func(r *checkout.Result) { r.Page.Plan = "pro" }},
		{"wrong country", "CHECKOUT_UI_UNSUPPORTED", func(r *checkout.Result) { r.Page.Country = "US" }},
		{"missing price", "CHECKOUT_UI_UNSUPPORTED", func(r *checkout.Result) { r.Page.DisplayedAmount = nil }},
		{"unapproved price", "CHECKOUT_UI_UNSUPPORTED", func(r *checkout.Result) { amount := 201.0; r.Page.DisplayedAmount = &amount }},
		{"unknown state", "CHECKOUT_UI_UNSUPPORTED", func(r *checkout.Result) { r.Page.StateID = "PRIVATE-STATE" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := checkout.Result{Page: testPage()}
			tc.mutate(&result)
			report, err := Run(context.Background(), preparedInput(t), executorFunc(func(context.Context, checkout.Request) (checkout.Result, error) { return result, nil }))
			if SafeCode(err) != tc.code || report.RemoteVerified || report.PreflightPassed || report.Page != nil {
				t.Fatalf("unsafe executor outcome accepted: report=%+v error=%v", report, err)
			}
			assertReportSanitized(t, report)
		})
	}
}

func TestErrorsNeverExposeRawUpstreamData(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code string
	}{
		{&checkout.Error{ErrorCode: "CHECKOUT_PAGE_UNAVAILABLE", Message: "https://PRIVATE.invalid/token", Cause: errors.New("buyer@example.invalid")}, "CHECKOUT_PAGE_UNAVAILABLE"},
		{&checkout.Error{ErrorCode: "PRIVATE_UNKNOWN_CODE", Message: "PRIVATE-UPSTREAM"}, "PREFLIGHT_FAILED"},
		{errors.New("PRIVATE-UPSTREAM"), "PREFLIGHT_FAILED"},
	} {
		report, err := Run(context.Background(), preparedInput(t), executorFunc(func(context.Context, checkout.Request) (checkout.Result, error) { return checkout.Result{}, tc.err }))
		if SafeCode(err) != tc.code || err.Error() != tc.code {
			t.Fatalf("unsafe error normalization: %v", err)
		}
		report.Code = SafeCode(err)
		assertReportSanitized(t, report)
	}
}

func TestCancelledRunDoesNotCallExecutor(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Run(ctx, preparedInput(t), executorFunc(func(context.Context, checkout.Request) (checkout.Result, error) {
		t.Fatal("cancelled context reached the executor")
		return checkout.Result{}, nil
	}))
	if SafeCode(err) != "PREFLIGHT_CANCELLED" {
		t.Fatalf("cancelled result=%v", err)
	}
}

func TestInspectSessionReportsOnlyKnownShapeAndValidExpiry(t *testing.T) {
	raw := `{"sessionToken":"PRIVATE-SESSION-TOKEN","accessToken":"PRIVATE-ACCESS-TOKEN","id":"PRIVATE-ROOT-ID",` +
		`"user":{"email":"buyer@example.invalid"},"account":{"id":"PRIVATE-ACCOUNT-ID"},` +
		`"PRIVATE-UNKNOWN-KEY":"PRIVATE-UNKNOWN-VALUE","expires":"2099-01-01T01:00:00+01:00"}`
	report, err := InspectSession(strings.NewReader(raw), testNow)
	if err != nil || report.RemoteVerified || report.InputValid || report.PaymentAllowed || report.SessionShape == nil ||
		report.SessionShape.Expires != "2099-01-01T00:00:00Z" || report.SessionShape.ExpiryStatus != "future" {
		t.Fatalf("inspection result=%+v err=%v", report, err)
	}
	if report.SessionShape.Fields["user.id"].Present || report.SessionShape.Fields["user.email"].Type != "string" {
		t.Fatal("nested field presence inherited a root identity or missed an existing field")
	}
	assertReportSanitized(t, report)
	report, err = InspectSession(strings.NewReader(`{"accessToken":"PRIVATE-ACCESS","expires":"PRIVATE-BAD-TIME"}`), testNow)
	if err != nil || report.SessionShape.Expires != "" || report.SessionShape.ExpiryStatus != "invalid" || report.SessionShape.Fields["sessionToken"].Present {
		t.Fatal("access-token-only inspection was misrepresented")
	}
	assertReportSanitized(t, report)
	for _, raw := range []string{`null`, `[]`, `{} {}`, `{"user":`} {
		if _, err := InspectSession(strings.NewReader(raw), testNow); err == nil {
			t.Fatal("invalid Session object accepted")
		}
	}
}

func TestBrowserConfigurationDoesNotLaunchOrLeakValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-real-browser")
	if err := os.WriteFile(path, []byte("not executable browser content"), 0700); err != nil {
		t.Fatal(err)
	}
	valid := BrowserOptions{ChromePath: path, Timeout: time.Minute}
	if _, err := NewBrowserExecutor(valid); err != nil {
		t.Fatalf("constructing executor unexpectedly ran browser content: %v", err)
	}
	for _, mutate := range []func(*BrowserOptions){
		func(o *BrowserOptions) { o.ChromePath = "PRIVATE-MISSING-PATH" },
		func(o *BrowserOptions) { o.Timeout = 0 },
		func(o *BrowserOptions) { o.ProxyServer = "http://user:PRIVATE-PASSWORD@127.0.0.1:8080" },
		func(o *BrowserOptions) { o.ProxyServer = "https://127.0.0.1:8080" },
		func(o *BrowserOptions) { o.ProxyServer = "http://127.0.0.1:8080/?token=PRIVATE" },
	} {
		options := valid
		mutate(&options)
		_, err := NewBrowserExecutor(options)
		if SafeCode(err) != "PREFLIGHT_BROWSER_CONFIG_INVALID" || strings.Contains(err.Error(), "PRIVATE") {
			t.Fatalf("invalid browser config accepted or echoed: %v", err)
		}
	}
}

func assertReportSanitized(t *testing.T, report Report) {
	t.Helper()
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"PRIVATE", "buyer@example.invalid", "another@example.invalid"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatal("diagnostic output contains a private input or upstream value")
		}
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("PRIVATE-READER") }

func TestInputReaderErrorIsSanitized(t *testing.T) {
	_, err := Decode(failingReader{}, testNow)
	if err == nil || err.Error() != "PREFLIGHT_INPUT_INVALID" || errors.Is(err, io.EOF) {
		t.Fatalf("reader error leaked: %v", err)
	}
}
