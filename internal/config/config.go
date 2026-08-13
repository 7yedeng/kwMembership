package config

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultPollInterval = 5 * time.Second
	DefaultHTTPTimeout  = 15 * time.Second
	defaultJWTSecret    = "replace-with-a-long-random-string"
)

type Config struct {
	ProjectRoot           string
	KawangRoot            string
	DatabasePath          string
	MaintenancePath       string
	EncryptionKey         string
	PollInterval          time.Duration
	HTTPTimeout           time.Duration
	APIURL                string
	CheckoutExecutor      string
	ChromePath            string
	ChromeProxyServer     string
	VisibleBrowser        bool
	BrowserTimeout        time.Duration
	HumanChallengeTimeout time.Duration
	ExecutorListenAddress string
	ExecutorSecret        string
	LeaseTTL              time.Duration
	HeartbeatInterval     time.Duration
}

func Load() (Config, error) {
	root, err := projectRoot()
	if err != nil {
		return Config{}, err
	}
	if err := loadDotenv(filepath.Join(root, ".env")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Config{}, fmt.Errorf("load .env: %w", err)
	}
	kawangRoot := env("KAWANG_PROJECT_ROOT", filepath.Join(root, "..", "kwRedeem"))
	if !filepath.IsAbs(kawangRoot) {
		kawangRoot = filepath.Join(root, kawangRoot)
	}
	kawangRoot, err = filepath.Abs(kawangRoot)
	if err != nil {
		return Config{}, fmt.Errorf("resolve kwRedeem root: %w", err)
	}
	if _, err := os.Stat(filepath.Join(kawangRoot, "package.json")); err != nil {
		return Config{}, fmt.Errorf("KAWANG_PROJECT_ROOT is not a kwRedeem checkout: %w", err)
	}
	if err := loadDotenv(filepath.Join(kawangRoot, ".env")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Config{}, fmt.Errorf("load kwRedeem .env: %w", err)
	}

	databasePath := env("DATABASE_PATH", "./data/kawang.db")
	if !filepath.IsAbs(databasePath) {
		databasePath = filepath.Join(kawangRoot, databasePath)
	}
	maintenancePath := env("MAINTENANCE_PATH", "./data/maintenance.json")
	if !filepath.IsAbs(maintenancePath) {
		maintenancePath = filepath.Join(kawangRoot, maintenancePath)
	}
	poll, err := milliseconds("WORKER_POLL_MS", DefaultPollInterval)
	if err != nil {
		return Config{}, err
	}
	timeout, err := milliseconds("DEFAULT_REQUEST_TIMEOUT_MS", DefaultHTTPTimeout)
	if err != nil {
		return Config{}, err
	}
	secret := env("JWT_SECRET", defaultJWTSecret)
	if strings.TrimSpace(secret) == defaultJWTSecret {
		return Config{}, fmt.Errorf("kwRedeem JWT_SECRET must replace the default placeholder")
	}
	if len(secret) < 12 {
		return Config{}, fmt.Errorf("kwRedeem JWT_SECRET is too short")
	}
	checkoutExecutor, err := validateCheckoutExecutor(env("KWMEMBERSHIP_CHECKOUT_EXECUTOR", "python"))
	if err != nil {
		return Config{}, err
	}
	chromePath := ""
	if checkoutExecutor == "legacy-go" {
		chromePath, err = resolveChromePath(env("KWMEMBERSHIP_CHROME_PATH", ""))
		if err != nil {
			return Config{}, err
		}
	}
	chromeProxyServer, err := validateChromeProxyServer(env("KWMEMBERSHIP_CHROME_PROXY_SERVER", ""))
	if err != nil {
		return Config{}, err
	}
	visibleBrowser, err := boolean("KWMEMBERSHIP_VISIBLE_BROWSER", false)
	if err != nil {
		return Config{}, err
	}
	browserTimeout, err := milliseconds("KWMEMBERSHIP_BROWSER_TIMEOUT_MS", 90*time.Second)
	if err != nil {
		return Config{}, err
	}
	humanChallengeTimeout, err := milliseconds("KWMEMBERSHIP_HUMAN_CHALLENGE_TIMEOUT_MS", 5*time.Minute)
	if err != nil {
		return Config{}, err
	}
	if humanChallengeTimeout != 5*time.Minute {
		return Config{}, fmt.Errorf("KWMEMBERSHIP_HUMAN_CHALLENGE_TIMEOUT_MS must be exactly 300000")
	}
	executorListenAddress, err := validateExecutorListenAddress(env("KWMEMBERSHIP_EXECUTOR_LISTEN", "127.0.0.1:4312"))
	if err != nil {
		return Config{}, err
	}
	executorSecret := strings.TrimSpace(env("KWMEMBERSHIP_EXECUTOR_SECRET", ""))
	if len(executorSecret) < 32 || strings.ContainsAny(executorSecret, "\r\n\t ") {
		return Config{}, fmt.Errorf("KWMEMBERSHIP_EXECUTOR_SECRET must be at least 32 non-whitespace characters")
	}
	return Config{
		ProjectRoot:           root,
		KawangRoot:            filepath.Clean(kawangRoot),
		DatabasePath:          filepath.Clean(databasePath),
		MaintenancePath:       filepath.Clean(maintenancePath),
		EncryptionKey:         secret,
		PollInterval:          poll,
		HTTPTimeout:           timeout,
		APIURL:                strings.TrimRight(env("API_URL", "http://127.0.0.1:4300"), "/"),
		CheckoutExecutor:      checkoutExecutor,
		ChromePath:            chromePath,
		ChromeProxyServer:     chromeProxyServer,
		VisibleBrowser:        visibleBrowser,
		BrowserTimeout:        browserTimeout,
		HumanChallengeTimeout: humanChallengeTimeout,
		ExecutorListenAddress: executorListenAddress,
		ExecutorSecret:        executorSecret,
		LeaseTTL:              20 * time.Second,
		HeartbeatInterval:     5 * time.Second,
	}, nil
}

func validateCheckoutExecutor(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value != "python" && value != "legacy-go" {
		return "", fmt.Errorf("KWMEMBERSHIP_CHECKOUT_EXECUTOR must be python or legacy-go")
	}
	return value, nil
}

func validateExecutorListenAddress(value string) (string, error) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(value))
	if err != nil {
		return "", fmt.Errorf("KWMEMBERSHIP_EXECUTOR_LISTEN must be a loopback host:port")
	}
	ip := net.ParseIP(host)
	parsedPort, portErr := strconv.Atoi(port)
	if ip == nil || !ip.IsLoopback() || portErr != nil || parsedPort < 1 || parsedPort > 65535 {
		return "", fmt.Errorf("KWMEMBERSHIP_EXECUTOR_LISTEN must use a loopback IP and valid port")
	}
	return net.JoinHostPort(host, port), nil
}

func validateChromeProxyServer(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	parsed, err := url.Parse(value)
	port, portErr := strconv.Atoi(parsed.Port())
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "socks5") || parsed.User != nil ||
		parsed.Hostname() == "" || portErr != nil || port < 1 || port > 65535 ||
		parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("KWMEMBERSHIP_CHROME_PROXY_SERVER must be an http:// or socks5:// host:port URL without credentials")
	}
	return value, nil
}

func resolveChromePath(configured string) (string, error) {
	configured = strings.TrimSpace(configured)
	if configured != "" {
		path, err := filepath.Abs(configured)
		if err != nil {
			return "", fmt.Errorf("resolve KWMEMBERSHIP_CHROME_PATH: %w", err)
		}
		if info, err := os.Stat(path); err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			return "", fmt.Errorf("KWMEMBERSHIP_CHROME_PATH is not an executable file: %s", path)
		}
		return path, nil
	}
	for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"} {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	macChrome := "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
	if info, err := os.Stat(macChrome); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
		return macChrome, nil
	}
	return "", fmt.Errorf("headless Chrome/Chromium was not found; set KWMEMBERSHIP_CHROME_PATH")
}

func projectRoot() (string, error) {
	if configured := strings.TrimSpace(os.Getenv("KWMEMBERSHIP_PROJECT_ROOT")); configured != "" {
		return filepath.Abs(configured)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve cwd: %w", err)
	}
	for current := cwd; ; current = filepath.Dir(current) {
		if _, err := os.Stat(filepath.Join(current, "go.mod")); err == nil {
			return current, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	return "", fmt.Errorf("cannot find kwMembership project root from %s", cwd)
}

func loadDotenv(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		key, raw, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" || os.Getenv(key) != "" {
			continue
		}
		value := strings.TrimSpace(raw)
		if len(value) >= 2 && ((value[0] == '\'' && value[len(value)-1] == '\'') || (value[0] == '"' && value[len(value)-1] == '"')) {
			quote := value[0]
			value = value[1 : len(value)-1]
			if quote == '"' {
				value = strings.NewReplacer(`\n`, "\n", `\r`, "\r", `\t`, "\t", `\\`, `\`).Replace(value)
			}
		} else if before, _, found := strings.Cut(value, " #"); found {
			value = strings.TrimSpace(before)
		}
		if err := os.Setenv(key, value); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func env(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

func milliseconds(key string, fallback time.Duration) (time.Duration, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive millisecond value", key)
	}
	return time.Duration(value) * time.Millisecond, nil
}

func boolean(key string, fallback bool) (bool, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	value, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false, fmt.Errorf("%s must be true or false", key)
	}
	return value, nil
}
