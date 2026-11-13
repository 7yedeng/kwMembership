// Package sessionrestore owns the credential-to-identity acceptance step only.
// It does not create checkouts, access card platforms, or change subscriptions.
package sessionrestore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"kwmembership/internal/provider"
)

const MaxSessionBytes = 256 << 10

var tokenCharacters = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)

type Error struct{ code string }

func (e *Error) Error() string  { return e.code }
func (e *Error) Code() string   { return e.code }
func failure(code string) error { return &Error{code: code} }

var safeCodes = map[string]bool{
	"SESSION_INPUT_INVALID": true, "SESSION_INPUT_TOO_LARGE": true,
	"SESSION_TOKEN_MISSING": true, "SESSION_TOKEN_INVALID": true,
	"SESSION_EXPIRED": true, "SESSION_IDENTITY_INVALID": true,
	"SESSION_IDENTITY_MISSING": true, "SESSION_BROWSER_CONFIG_INVALID": true,
	"SESSION_BROWSER_UNAVAILABLE": true, "SESSION_COOKIE_WRITE_FAILED": true,
	"SESSION_COOKIE_READBACK_FAILED": true, "SESSION_COOKIE_MISMATCH": true,
	"SESSION_NAVIGATION_FAILED": true, "SESSION_IDENTITY_UNAVAILABLE": true,
	"SESSION_IDENTITY_UNCONFIRMED": true, "SESSION_IDENTITY_MISMATCH": true,
	"SESSION_NOT_LOGGED_IN": true, "SESSION_REJECTED": true,
	"SESSION_SITE_DENIED": true, "SESSION_ORIGIN_MISMATCH": true,
	"SESSION_PROFILE_CLEANUP_FAILED": true, "SESSION_CANCELLED": true,
	"SESSION_TIMEOUT": true, "SESSION_ARGUMENTS_INVALID": true,
	"SESSION_UNEXPECTED": true,
}

// SafeCode never includes a browser, provider, URL, argument or credential error.
func SafeCode(err error) string {
	if errors.Is(err, context.Canceled) {
		return "SESSION_CANCELLED"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "SESSION_TIMEOUT"
	}
	var coded interface{ Code() string }
	if errors.As(err, &coded) && safeCodes[coded.Code()] {
		return coded.Code()
	}
	return "SESSION_UNEXPECTED"
}

type Identity struct {
	UserID string
	Email  string
}

// Prepared intentionally has no exported credential fields. JSON/debug output
// must not expose a Session or its cookie values.
type Prepared struct {
	identity Identity
	cookies  []provider.SessionCookie
}

func (Prepared) String() string   { return "[protected session]" }
func (Prepared) GoString() string { return "[protected session]" }

func Parse(raw []byte, now time.Time) (Prepared, error) {
	if len(raw) > MaxSessionBytes {
		return Prepared{}, failure("SESSION_INPUT_TOO_LARGE")
	}
	raw = bytes.TrimSpace(raw)
	var payload struct {
		SessionToken string `json:"sessionToken"`
		Email        string `json:"email"`
		User         struct {
			ID    string `json:"id"`
			Email string `json:"email"`
		} `json:"user"`
	}
	if len(raw) == 0 || raw[0] != '{' || json.Unmarshal(raw, &payload) != nil {
		return Prepared{}, failure("SESSION_INPUT_INVALID")
	}
	if payload.SessionToken == "" {
		return Prepared{}, failure("SESSION_TOKEN_MISSING")
	}
	if len(payload.SessionToken) < 40 || len(payload.SessionToken) > 65536 || !tokenCharacters.MatchString(payload.SessionToken) {
		return Prepared{}, failure("SESSION_TOKEN_INVALID")
	}
	if !validUserID(payload.User.ID) {
		return Prepared{}, failure("SESSION_IDENTITY_INVALID")
	}
	if payload.Email != "" && payload.User.Email != "" &&
		!strings.EqualFold(strings.TrimSpace(payload.Email), strings.TrimSpace(payload.User.Email)) {
		return Prepared{}, failure("SESSION_IDENTITY_INVALID")
	}
	session, err := provider.BrowserSessionFromJSON(raw, now)
	if err != nil {
		var code interface{ Code() string }
		if errors.As(err, &code) {
			switch code.Code() {
			case "CHATGPT_SESSION_UNAUTHORIZED":
				return Prepared{}, failure("SESSION_EXPIRED")
			case "EXPECTED_IDENTITY_MISSING":
				return Prepared{}, failure("SESSION_IDENTITY_MISSING")
			case "SESSION_COOKIE_MISSING":
				return Prepared{}, failure("SESSION_TOKEN_MISSING")
			}
		}
		return Prepared{}, failure("SESSION_INPUT_INVALID")
	}
	// This acceptance tool restores session cookies only; it does not extend
	// server validity or persist the supplied expiry in the browser's jar.
	for i := range session.Cookies {
		session.Cookies[i].Expiration = nil
	}
	return Prepared{identity: Identity{UserID: payload.User.ID, Email: session.Email}, cookies: session.Cookies}, nil
}

func validUserID(value string) bool {
	if len(value) > 512 || strings.TrimSpace(value) != value {
		return false
	}
	for _, char := range value {
		if char < 32 || char == 127 {
			return false
		}
	}
	return true
}

type Report struct {
	Operation              string `json:"operation"`
	Code                   string `json:"code"`
	Stage                  string `json:"stage"`
	RemoteVerified         bool   `json:"remoteVerified"`
	CookieReadbackVerified bool   `json:"cookieReadbackVerified"`
	IdentityMatch          bool   `json:"identityMatch"`
	UserIDRequired         bool   `json:"userIdRequired"`
	TargetFingerprint      string `json:"targetFingerprint,omitempty"`
	BrowserOpened          bool   `json:"browserOpened"`
	ProfileRemoved         bool   `json:"profileRemoved"`
	PaymentAllowed         bool   `json:"paymentAllowed"`
}

func Failure(operation, code string) Report {
	if !safeCodes[code] {
		code = "SESSION_UNEXPECTED"
	}
	return Report{Operation: operation, Code: code, Stage: "input"}
}

func Inspect(session Prepared) Report {
	sum := sha256.Sum256([]byte("session-identity-v1\x00" + session.identity.UserID + "\x00" + session.identity.Email))
	return Report{Operation: "inspect", Code: "SESSION_INSPECTED", Stage: "input_validated",
		UserIDRequired: session.identity.UserID != "", TargetFingerprint: hex.EncodeToString(sum[:])[:16]}
}

type Options struct {
	ChromePath  string
	ShowBrowser bool
	Timeout     time.Duration
	HoldOpen    time.Duration
}

func (options Options) Validate() error {
	if options.Timeout < time.Second || options.Timeout > 3*time.Minute || options.HoldOpen < 0 ||
		options.HoldOpen > 5*time.Minute || options.HoldOpen > 0 && !options.ShowBrowser {
		return failure("SESSION_BROWSER_CONFIG_INVALID")
	}
	return nil
}

type Cookie struct {
	Name        string
	Value       string
	Domain      string
	Path        string
	Secure      bool
	HTTPOnly    bool
	SameSite    string
	Session     bool
	Partitioned bool
}

// Observation contains identity facts only, never access tokens or the raw API body.
type Observation struct {
	Code   string
	UserID string
	Email  string
}

type Browser interface {
	WriteCookies(context.Context, []provider.SessionCookie) error
	ReadCookies(context.Context) ([]Cookie, error)
	Navigate(context.Context) error
	Observe(context.Context) (Observation, error)
	Close() error
}

type Factory func(context.Context, Options) (Browser, error)
type Progress func(string)

func cookiesMatch(expected []provider.SessionCookie, actual []Cookie) bool {
	if len(expected) == 0 || len(expected) != len(actual) {
		return false
	}
	seen := make(map[string]bool, len(actual))
	for _, cookie := range actual {
		if seen[cookie.Name] {
			return false
		}
		seen[cookie.Name] = true
		matched := false
		for _, want := range expected {
			if want.Name == cookie.Name {
				matched = want.Value == cookie.Value && want.Domain == cookie.Domain && want.Path == cookie.Path &&
					cookie.Secure && cookie.HTTPOnly && cookie.SameSite == "lax" && cookie.Session && !cookie.Partitioned
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func verifyIdentity(expected Identity, actual Observation) error {
	if actual.Code != "" {
		switch actual.Code {
		case "SESSION_NOT_LOGGED_IN", "SESSION_REJECTED", "SESSION_EXPIRED", "SESSION_SITE_DENIED", "SESSION_ORIGIN_MISMATCH", "SESSION_IDENTITY_UNAVAILABLE":
			return failure(actual.Code)
		default:
			return failure("SESSION_IDENTITY_UNAVAILABLE")
		}
	}
	if actual.Email == "" || expected.UserID != "" && actual.UserID == "" {
		return failure("SESSION_IDENTITY_UNCONFIRMED")
	}
	if !validUserID(actual.UserID) || strings.ToLower(strings.TrimSpace(actual.Email)) != expected.Email ||
		expected.UserID != "" && actual.UserID != expected.UserID {
		return failure("SESSION_IDENTITY_MISMATCH")
	}
	return nil
}

// Run does not expose credentials to progress or reports. It always closes its
// own browser; it never attaches to an existing personal profile or worker.
func Run(parent context.Context, session Prepared, options Options, factory Factory, progress Progress) (report Report, err error) {
	report = Inspect(session)
	report.Operation = "restore"
	stage := func(value string) {
		report.Stage = value
		if progress != nil {
			progress(value)
		}
	}
	defer func() {
		if err != nil {
			report.Code = SafeCode(err)
		}
	}()
	if options.Validate() != nil || factory == nil || len(session.cookies) == 0 || session.identity.Email == "" {
		return report, failure("SESSION_BROWSER_CONFIG_INVALID")
	}
	ctx, cancel := context.WithTimeout(parent, options.Timeout+options.HoldOpen)
	defer cancel()
	operation, cancelOperation := context.WithTimeout(ctx, options.Timeout)
	defer cancelOperation()
	stage("browser_starting")
	browser, startErr := factory(ctx, options)
	if startErr != nil || browser == nil {
		if browser != nil {
			_ = browser.Close()
		}
		if operation.Err() != nil {
			return report, operation.Err()
		}
		if SafeCode(startErr) == "SESSION_BROWSER_CONFIG_INVALID" {
			return report, failure("SESSION_BROWSER_CONFIG_INVALID")
		}
		return report, failure("SESSION_BROWSER_UNAVAILABLE")
	}
	report.BrowserOpened = true
	defer func() {
		if closeErr := browser.Close(); closeErr != nil {
			err = failure("SESSION_PROFILE_CLEANUP_FAILED")
		} else {
			report.ProfileRemoved = true
			if err == nil {
				stage("browser_closed")
			}
		}
	}()
	call := func(code string, fn func() error) error {
		if operation.Err() != nil {
			return operation.Err()
		}
		if callErr := fn(); callErr != nil {
			if operation.Err() != nil {
				return operation.Err()
			}
			return failure(code)
		}
		return nil
	}
	stage("cookie_writing")
	if err = call("SESSION_COOKIE_WRITE_FAILED", func() error { return browser.WriteCookies(operation, session.cookies) }); err != nil {
		return report, err
	}
	stage("cookie_readback")
	var actual []Cookie
	if err = call("SESSION_COOKIE_READBACK_FAILED", func() error { var e error; actual, e = browser.ReadCookies(operation); return e }); err != nil {
		return report, err
	}
	if !cookiesMatch(session.cookies, actual) {
		return report, failure("SESSION_COOKIE_MISMATCH")
	}
	report.CookieReadbackVerified = true
	stage("navigating")
	if err = call("SESSION_NAVIGATION_FAILED", func() error { return browser.Navigate(operation) }); err != nil {
		return report, err
	}
	stage("identity_checking")
	var observation Observation
	if err = call("SESSION_IDENTITY_UNAVAILABLE", func() error { var e error; observation, e = browser.Observe(operation); return e }); err != nil {
		return report, err
	}
	if err = verifyIdentity(session.identity, observation); err != nil {
		return report, err
	}
	report.RemoteVerified, report.IdentityMatch = true, true
	report.Code = "SESSION_VERIFIED"
	stage("identity_verified")
	if options.HoldOpen > 0 {
		stage("browser_visible")
		timer := time.NewTimer(options.HoldOpen)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return report, ctx.Err()
		}
	}
	stage("closing_browser")
	return report, nil
}

// Check that even common formatting paths cannot accidentally serialize tokens.
var _ fmt.Stringer = Prepared{}
