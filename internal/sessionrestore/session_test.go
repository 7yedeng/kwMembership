package sessionrestore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"kwmembership/internal/provider"
)

var testNow = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)

func sessionJSON() []byte {
	return []byte(`{"sessionToken":"` + strings.Repeat("synthetic-only-", 350) + `","user":{"id":"user_synthetic","email":"owner@example.invalid"},"expires":"2031-01-01T00:00:00Z"}`)
}

func prepared(t *testing.T) Prepared {
	t.Helper()
	session, err := Parse(sessionJSON(), testNow)
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func TestParseAndReportDoNotExposeCredentials(t *testing.T) {
	session := prepared(t)
	if len(session.cookies) != 2 {
		t.Fatal("chunked token was not preserved")
	}
	var joined strings.Builder
	for _, cookie := range session.cookies {
		joined.WriteString(cookie.Value)
		if cookie.Expiration != nil {
			t.Fatal("acceptance cookies must be session-only")
		}
	}
	if joined.String() != strings.Repeat("synthetic-only-", 350) {
		t.Fatal("token changed")
	}
	for _, value := range []any{session, Inspect(session)} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		for _, rendered := range []string{string(encoded), fmt.Sprintf("%v", value), fmt.Sprintf("%#v", value)} {
			if strings.Contains(rendered, "synthetic-only-") || strings.Contains(rendered, "owner@example.invalid") {
				t.Fatal("credential or direct identity leaked")
			}
		}
	}
	if Inspect(session).RemoteVerified || Inspect(session).PaymentAllowed {
		t.Fatal("inspection claimed remote or payment permission")
	}
}

func TestParseRejectsAmbiguousOrIncompleteMaterial(t *testing.T) {
	tests := []struct{ name, value, code string }{
		{"access-only", `{"accessToken":"opaque","user":{"email":"owner@example.invalid"}}`, "SESSION_TOKEN_MISSING"},
		{"expired", strings.Replace(string(sessionJSON()), "2031-01-01", "2020-01-01", 1), "SESSION_EXPIRED"},
		{"conflicting-email", strings.Replace(string(sessionJSON()), `"user":`, `"email":"other@example.invalid","user":`, 1), "SESSION_IDENTITY_INVALID"},
		{"identity-type", strings.Replace(string(sessionJSON()), `"user_synthetic"`, `42`, 1), "SESSION_INPUT_INVALID"},
		{"token-invalid", strings.Replace(string(sessionJSON()), "synthetic-only-", "has space", 1), "SESSION_TOKEN_INVALID"},
		{"missing-email", strings.Replace(string(sessionJSON()), `"email":"owner@example.invalid"`, `"email":""`, 1), "SESSION_IDENTITY_MISSING"},
		{"huge", strings.Repeat("x", MaxSessionBytes+1), "SESSION_INPUT_TOO_LARGE"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Parse([]byte(test.value), testNow)
			if SafeCode(err) != test.code {
				t.Fatalf("got %s want %s", SafeCode(err), test.code)
			}
		})
	}
}

type fakeBrowser struct {
	cookies     []Cookie
	observation Observation
	events      []string
	corrupt     func([]Cookie) []Cookie
	failStage   string
	closeError  bool
	onObserve   func(context.Context)
}

func (b *fakeBrowser) WriteCookies(_ context.Context, cookies []provider.SessionCookie) error {
	b.events = append(b.events, "write")
	if b.failStage == "write" {
		return errors.New("raw secret synthetic-only-token")
	}
	for _, cookie := range cookies {
		b.cookies = append(b.cookies, Cookie{Name: cookie.Name, Value: cookie.Value,
			Domain: cookie.Domain, Path: cookie.Path, Secure: true, HTTPOnly: true, SameSite: "lax", Session: true})
	}
	return nil
}
func (b *fakeBrowser) ReadCookies(context.Context) ([]Cookie, error) {
	b.events = append(b.events, "read")
	if b.failStage == "read" {
		return nil, errors.New("raw secret synthetic-only-token")
	}
	if b.corrupt != nil {
		return b.corrupt(b.cookies), nil
	}
	return b.cookies, nil
}
func (b *fakeBrowser) Navigate(context.Context) error {
	b.events = append(b.events, "navigate")
	if b.failStage == "navigate" {
		return errors.New("raw secret synthetic-only-token")
	}
	return nil
}
func (b *fakeBrowser) Observe(ctx context.Context) (Observation, error) {
	b.events = append(b.events, "observe")
	if b.onObserve != nil {
		b.onObserve(ctx)
	}
	if ctx.Err() != nil {
		return Observation{}, ctx.Err()
	}
	if b.failStage == "observe" {
		return Observation{}, errors.New("raw secret synthetic-only-token")
	}
	return b.observation, nil
}
func (b *fakeBrowser) Close() error {
	b.events = append(b.events, "close")
	if b.closeError {
		return errors.New("sensitive temporary profile path")
	}
	return nil
}

func successBrowser() *fakeBrowser {
	return &fakeBrowser{observation: Observation{Email: "owner@example.invalid", UserID: "user_synthetic"}}
}
func options() Options { return Options{Timeout: time.Second, ChromePath: "synthetic-browser"} }
func factory(b *fakeBrowser) Factory {
	return func(context.Context, Options) (Browser, error) { return b, nil }
}

func TestRestoreOrdersEvidenceAndCleanup(t *testing.T) {
	b := successBrowser()
	report, err := Run(context.Background(), prepared(t), options(), factory(b), nil)
	if err != nil || report.Code != "SESSION_VERIFIED" || !report.RemoteVerified || !report.IdentityMatch ||
		!report.CookieReadbackVerified || !report.ProfileRemoved || report.PaymentAllowed || report.Stage != "browser_closed" {
		t.Fatalf("unexpected result: %+v %v", report, err)
	}
	if strings.Join(b.events, ",") != "write,read,navigate,observe,close" {
		t.Fatal(b.events)
	}
}

func TestWrongAccountNeverPasses(t *testing.T) {
	for _, observation := range []Observation{
		{Email: "owner@example.invalid", UserID: "other-user"},
		{Email: "other@example.invalid", UserID: "user_synthetic"},
		{Email: "owner@example.invalid"},
		{Email: "owner@example.invalid", UserID: "user_synthetic", Code: "SESSION_REJECTED"},
		{Code: "SESSION_SITE_DENIED"},
	} {
		b := successBrowser()
		b.observation = observation
		report, err := Run(context.Background(), prepared(t), options(), factory(b), nil)
		if err == nil || report.RemoteVerified || report.IdentityMatch || !report.ProfileRemoved {
			t.Fatalf("wrong account passed: %+v %v", report, err)
		}
	}
}

func TestCookieReadbackMustBeExactBeforeNavigation(t *testing.T) {
	mutations := map[string]func([]Cookie) []Cookie{
		"value":         func(c []Cookie) []Cookie { c[0].Value += "x"; return c },
		"missing":       func(c []Cookie) []Cookie { return c[:1] },
		"extra":         func(c []Cookie) []Cookie { return append(c, c[0]) },
		"not-http-only": func(c []Cookie) []Cookie { c[0].HTTPOnly = false; return c },
		"host-only":     func(c []Cookie) []Cookie { c[0].Domain = "chatgpt.com"; return c },
		"partitioned":   func(c []Cookie) []Cookie { c[0].Partitioned = true; return c },
		"persisted":     func(c []Cookie) []Cookie { c[0].Session = false; return c },
	}
	for name, mutation := range mutations {
		t.Run(name, func(t *testing.T) {
			b := successBrowser()
			b.corrupt = mutation
			report, err := Run(context.Background(), prepared(t), options(), factory(b), nil)
			if SafeCode(err) != "SESSION_COOKIE_MISMATCH" || report.CookieReadbackVerified || !report.ProfileRemoved {
				t.Fatal(report, err)
			}
			if strings.Join(b.events, ",") != "write,read,close" {
				t.Fatal("navigated before cookie verification", b.events)
			}
		})
	}
}

func TestStageErrorsAreRedactedAndClosed(t *testing.T) {
	for _, stage := range []string{"write", "read", "navigate", "observe"} {
		t.Run(stage, func(t *testing.T) {
			b := successBrowser()
			b.failStage = stage
			report, err := Run(context.Background(), prepared(t), options(), factory(b), nil)
			if err == nil || !report.ProfileRemoved || strings.Contains(fmt.Sprint(err, report), "synthetic-only") {
				t.Fatal(report, err)
			}
		})
	}
}

func TestCleanupFailureCannotReportSuccess(t *testing.T) {
	b := successBrowser()
	b.closeError = true
	report, err := Run(context.Background(), prepared(t), options(), factory(b), nil)
	if SafeCode(err) != "SESSION_PROFILE_CLEANUP_FAILED" || report.Code != "SESSION_PROFILE_CLEANUP_FAILED" || report.ProfileRemoved {
		t.Fatal(report, err)
	}
}

func TestCancellationClosesBrowser(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	b := successBrowser()
	b.onObserve = func(ctx context.Context) { cancel(); <-ctx.Done() }
	report, err := Run(ctx, prepared(t), options(), factory(b), nil)
	if SafeCode(err) != "SESSION_CANCELLED" || !report.ProfileRemoved || report.RemoteVerified {
		t.Fatal(report, err)
	}
}

func TestDisplayDoesNotChangeIdentityRequirements(t *testing.T) {
	for _, show := range []bool{false, true} {
		b := successBrowser()
		b.observation.UserID = "different"
		opt := options()
		opt.ShowBrowser = show
		report, err := Run(context.Background(), prepared(t), opt, factory(b), nil)
		if SafeCode(err) != "SESSION_IDENTITY_MISMATCH" || report.RemoteVerified {
			t.Fatal(report, err)
		}
	}
	opt := options()
	opt.HoldOpen = time.Minute
	if opt.Validate() == nil {
		t.Fatal("invisible hold was accepted")
	}
}
