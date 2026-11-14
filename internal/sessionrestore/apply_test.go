package sessionrestore

import (
	"context"
	"strings"
	"testing"

	"kwmembership/internal/provider"
)

func TestApplyRetainsCallerBrowserForSubsequentTaskSteps(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	browser := successBrowser()
	report, err := Apply(ctx, prepared(t), browser, nil)
	if err != nil || report.Operation != "apply" || report.Code != "SESSION_VERIFIED" ||
		report.Stage != "identity_verified" || !report.CookieReadbackVerified || !report.IdentityMatch || !report.RemoteVerified ||
		report.BrowserOpened || report.ProfileRemoved || report.PaymentAllowed {
		t.Fatalf("unexpected embedded authentication result: %+v %v", report, err)
	}
	if ctx.Err() != nil || strings.Join(browser.events, ",") != "write,read,navigate,observe" {
		t.Fatal("authentication ended the caller-owned lifecycle", browser.events)
	}
	if _, err := browser.Observe(ctx); err != nil {
		t.Fatal("the next task step could not use the browser")
	}
	if err := browser.Close(); err != nil || strings.Join(browser.events, ",") != "write,read,navigate,observe,observe,close" {
		t.Fatal("caller cleanup did not remain independent", browser.events)
	}
}

func TestApplyFailureRetainsOwnershipAndStrictEvidence(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*fakeBrowser)
		code   string
		events string
	}{
		{"persisted-cookie", func(b *fakeBrowser) {
			b.corrupt = func(c []Cookie) []Cookie { c[0].Session = false; return c }
		}, "SESSION_COOKIE_MISMATCH", "write,read"},
		{"same-email-wrong-id", func(b *fakeBrowser) { b.observation.UserID = "another-user" }, "SESSION_IDENTITY_MISMATCH", "write,read,navigate,observe"},
		{"same-id-wrong-email", func(b *fakeBrowser) { b.observation.Email = "another@example.invalid" }, "SESSION_IDENTITY_MISMATCH", "write,read,navigate,observe"},
		{"missing-required-id", func(b *fakeBrowser) { b.observation.UserID = "" }, "SESSION_IDENTITY_UNCONFIRMED", "write,read,navigate,observe"},
		{"server-rejection", func(b *fakeBrowser) { b.observation.Code = "SESSION_REJECTED" }, "SESSION_REJECTED", "write,read,navigate,observe"},
		{"driver-sensitive-error", func(b *fakeBrowser) { b.failStage = "navigate" }, "SESSION_NAVIGATION_FAILED", "write,read,navigate"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			browser := successBrowser()
			test.mutate(browser)
			report, err := Apply(ctx, prepared(t), browser, nil)
			if SafeCode(err) != test.code || report.Code != test.code || report.RemoteVerified || report.IdentityMatch ||
				report.BrowserOpened || report.ProfileRemoved || report.PaymentAllowed || ctx.Err() != nil {
				t.Fatalf("failed authentication acquired ownership or passed: %+v %v", report, err)
			}
			if strings.Join(browser.events, ",") != test.events || strings.Contains(err.Error(), "synthetic-only") {
				t.Fatal("failure did not preserve expected safe boundary", browser.events)
			}
		})
	}
}

type cancelledObservationBrowser struct {
	*fakeBrowser
	cancel context.CancelFunc
}

func (b *cancelledObservationBrowser) Observe(context.Context) (Observation, error) {
	b.events = append(b.events, "observe")
	b.cancel()
	return b.observation, nil
}

func TestApplyCancellationCannotBecomeSuccessOrCloseOwner(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	browser := &cancelledObservationBrowser{fakeBrowser: successBrowser(), cancel: cancel}
	report, err := Apply(ctx, prepared(t), browser, nil)
	if SafeCode(err) != "SESSION_CANCELLED" || report.Code != "SESSION_CANCELLED" || report.RemoteVerified ||
		report.IdentityMatch || report.ProfileRemoved || report.PaymentAllowed ||
		strings.Join(browser.events, ",") != "write,read,navigate,observe" {
		t.Fatalf("racing cancellation was not preserved: %+v %v", report, err)
	}
	other := successBrowser()
	_, err = Apply(ctx, prepared(t), other, nil)
	if SafeCode(err) != "SESSION_CANCELLED" || len(other.events) != 0 {
		t.Fatal("already cancelled operation touched the browser", other.events)
	}
}

type mutatingWriteBrowser struct{ *fakeBrowser }

func (b *mutatingWriteBrowser) WriteCookies(ctx context.Context, cookies []provider.SessionCookie) error {
	cookies[0].Value = "changed-by-driver"
	return b.fakeBrowser.WriteCookies(ctx, cookies)
}

func TestApplyKeepsPreparedBindingImmutableAcrossDrivers(t *testing.T) {
	session := prepared(t)
	identity := session.Identity()
	identity.Email, identity.UserID = "changed@example.invalid", "changed-id"
	if session.Identity().Email != "owner@example.invalid" || session.Identity().UserID != "user_synthetic" {
		t.Fatal("identity accessor exposed mutable binding")
	}
	browser := &mutatingWriteBrowser{fakeBrowser: successBrowser()}
	report, err := Apply(context.Background(), session, browser, nil)
	if SafeCode(err) != "SESSION_COOKIE_MISMATCH" || report.CookieReadbackVerified || strings.Join(browser.events, ",") != "write,read" {
		t.Fatal("driver mutated its own expected readback contract", report, err)
	}
	// Reuse of the prepared credential must still verify the original target,
	// not a driver's altered value or a caller's modified Identity copy.
	report, err = Apply(context.Background(), session, successBrowser(), nil)
	if err != nil || !report.IdentityMatch || !report.CookieReadbackVerified {
		t.Fatal("a prior driver corrupted prepared material", report, err)
	}
}

func TestApplyInvalidArgumentsDoNotTouchBorrowedBrowser(t *testing.T) {
	browser := successBrowser()
	for _, test := range []struct {
		ctx     context.Context
		session Prepared
		browser SessionBrowser
	}{
		{nil, prepared(t), browser},
		{context.Background(), Prepared{}, browser},
		{context.Background(), prepared(t), nil},
	} {
		report, err := Apply(test.ctx, test.session, test.browser, nil)
		if SafeCode(err) != "SESSION_BROWSER_CONFIG_INVALID" || report.BrowserOpened || report.ProfileRemoved || len(browser.events) != 0 {
			t.Fatal("invalid embedded restore touched the browser", report, err)
		}
	}
}
