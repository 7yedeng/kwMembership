package checkout

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This opt-in acceptance test uses the production launch check, not a mock
// browser or a second set of Chrome flags. CI supplies a full browser and an
// isolated Xvfb display under a non-root user. VerifyChrome creates a fresh
// temporary profile, visits only about:blank, and evaluates navigator.userAgent.
func TestVerifyChromeHeadedRuntime(t *testing.T) {
	const pathVariable = "KWMEMBERSHIP_CHROME_RUNTIME_PATH"
	const requiredVariable = "KWMEMBERSHIP_REQUIRE_CHROME_RUNTIME_TESTS"
	path := os.Getenv(pathVariable)
	if strings.TrimSpace(path) == "" {
		if os.Getenv(requiredVariable) == "1" {
			t.Fatal("Chrome runtime acceptance is required, but KWMEMBERSHIP_CHROME_RUNTIME_PATH is missing")
		}
		t.Skip("Chrome runtime acceptance not configured; set KWMEMBERSHIP_CHROME_RUNTIME_PATH to a full browser, or require it with KWMEMBERSHIP_REQUIRE_CHROME_RUNTIME_TESTS=1")
	}
	// chromedp can inject --no-sandbox for UID 0. Running as root would test a
	// different security boundary from the required sandboxed deployment.
	if os.Getuid() == 0 || os.Geteuid() == 0 {
		t.Fatal("sandboxed Chrome runtime acceptance must run as a non-root user")
	}
	if strings.TrimSpace(os.Getenv("DISPLAY")) == "" {
		t.Fatal("headed Chrome runtime acceptance requires an existing DISPLAY, such as an isolated Xvfb display")
	}
	if !filepath.IsAbs(path) {
		t.Fatal("KWMEMBERSHIP_CHROME_RUNTIME_PATH must be an absolute path to the full browser executable")
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		t.Fatal("KWMEMBERSHIP_CHROME_RUNTIME_PATH is not an executable regular file")
	}

	// Leave five seconds of a 45-second CI test budget for allocator/process
	// cleanup. No Session, account, proxy override, or site URL enters this path.
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	if err := VerifyChrome(ctx, path, "", true); err != nil {
		t.Fatalf("production sandboxed headed Chrome launch on about:blank failed: %v", err)
	}
}
