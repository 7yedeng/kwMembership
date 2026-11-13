package sessionrestore

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"kwmembership/internal/provider"
)

// This opt-in test starts vendor Chrome on about:blank only. It exercises the
// real browser lifetime and cookie store with a synthetic, unusable value. It
// never calls Navigate/Observe, never opens ChatGPT, and never accepts secrets.
func TestSessionChromeCookieRuntimeLocal(t *testing.T) {
	if os.Getenv("KWMEMBERSHIP_REQUIRE_SESSION_RUNTIME_TESTS") != "1" {
		t.Skip("local Chrome runtime acceptance is opt-in")
	}
	path := os.Getenv("KWMEMBERSHIP_CHROME_RUNTIME_PATH")
	if path == "" {
		t.Fatal("LOCAL_CHROME_PATH_REQUIRED")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	browser, err := NewChrome(ctx, Options{ChromePath: path, ShowBrowser: true, Timeout: 15 * time.Second})
	if err != nil {
		t.Fatal(SafeCode(err))
	}
	defer browser.Close()
	owner := browser.(*chromeBrowser)
	profile := owner.profile
	borrowed, err := UseChromeContext(owner.ctx)
	if err != nil {
		t.Fatal("LOCAL_STARTED_CONTEXT_NOT_ACCEPTED")
	}
	if _, ownsLifecycle := borrowed.(interface{ Close() error }); ownsLifecycle {
		t.Fatal("LOCAL_BORROWED_BROWSER_EXPOSES_CLOSE")
	}
	cookies := []provider.SessionCookie{{
		Name: "__Secure-next-auth.session-token", Value: "synthetic-not-a-session-" + strings.Repeat("a", 48),
		Domain: ".chatgpt.com", Path: "/", SameSite: "lax", Secure: true, HTTPOnly: true,
	}}
	if err := borrowed.WriteCookies(ctx, cookies); err != nil {
		t.Fatal("LOCAL_COOKIE_WRITE_FAILED")
	}
	observed, err := borrowed.ReadCookies(ctx)
	if err != nil {
		t.Fatal("LOCAL_COOKIE_READ_FAILED")
	}
	if len(observed) != 1 || observed[0].Name != cookies[0].Name || observed[0].Value != cookies[0].Value ||
		observed[0].Domain != ".chatgpt.com" || observed[0].Path != "/" || !observed[0].Secure ||
		!observed[0].HTTPOnly || !observed[0].Session || observed[0].Partitioned || observed[0].SameSite != "lax" {
		t.Fatal("LOCAL_COOKIE_READBACK_MISMATCH")
	}
	// A second action after the first one returned catches allocator lifetime
	// errors that a fake Browser interface cannot detect.
	if _, err := browser.ReadCookies(ctx); err != nil || owner.ctx.Err() != nil {
		t.Fatal("LOCAL_BROWSER_DID_NOT_REMAIN_ALIVE")
	}
	if err := browser.Close(); err != nil {
		t.Fatal(SafeCode(err))
	}
	if _, err := os.Stat(profile); !os.IsNotExist(err) {
		t.Fatal("LOCAL_PROFILE_NOT_REMOVED")
	}
}
