package provider

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"sync"
)

// CardPlatformConfig is transient constructor input. The workflow owns the
// configuration key and decrypts the shared database record; only adapters
// interpret CredentialJSON. Neither credentials nor endpoints are printable.
type CardPlatformConfig struct {
	Key            string          `json:"key"`
	Kind           string          `json:"kind"`
	BaseURL        string          `json:"-"`
	CredentialJSON json.RawMessage `json:"-"`
}

func (CardPlatformConfig) String() string   { return "CardPlatformConfig{redacted}" }
func (CardPlatformConfig) GoString() string { return "CardPlatformConfig{redacted}" }

// CardPlatformFactory must only construct a client. It must not contact an
// upstream, open a card, fund it, or infer permission to perform a write.
type CardPlatformFactory func(*http.Client, CardPlatformConfig) (CardPlatform, error)

// ConfiguredCardPlatform keeps two identities separate. Key is the configured
// account key used by inventory, card bindings and funding intents. Kind is the
// implementation name. Multiple accounts of one kind must have different keys.
type ConfiguredCardPlatform interface {
	CardPlatform
	Kind() string
}

type configuredCardPlatform struct {
	CardPlatform
	key  string
	kind string
}

func (p *configuredCardPlatform) Key() string  { return p.key }
func (p *configuredCardPlatform) Kind() string { return p.kind }

// CardPlatformKind is for adapter-specific error classification only. Durable
// inventory, billing and funding scopes must use CardPlatform.Key instead.
// Direct legacy clients expose their kind through Key and remain compatible.
func CardPlatformKind(platform CardPlatform) string {
	if configured, ok := platform.(ConfiguredCardPlatform); ok {
		return configured.Kind()
	}
	return platform.Key()
}

// CardPlatformRegistry is a construction registry, not an execution queue or a
// client cache. Instances are fresh on every Create call. Register is intended
// for startup composition; an existing adapter cannot be replaced in place.
type CardPlatformRegistry struct {
	mu        sync.RWMutex
	factories map[string]CardPlatformFactory
}

var cardPlatformKindPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
var cardPlatformKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

// NewCardPlatformRegistry is the single built-in adapter composition point.
// Adding an adapter here does not require parsing its credentials in Processor.
func NewCardPlatformRegistry() *CardPlatformRegistry {
	return &CardPlatformRegistry{factories: map[string]CardPlatformFactory{
		CardPlatformSpaceX: newConfiguredSpaceX,
		CardPlatformEfun:   newConfiguredEfun,
	}}
}

func (r *CardPlatformRegistry) Register(kind string, factory CardPlatformFactory) error {
	if r == nil || !cardPlatformKindPattern.MatchString(kind) || factory == nil {
		return cardFactoryError("CARD_PLATFORM_REGISTRATION_INVALID")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.factories == nil {
		r.factories = make(map[string]CardPlatformFactory)
	}
	if _, found := r.factories[kind]; found {
		return cardFactoryError("CARD_PLATFORM_KIND_DUPLICATE")
	}
	r.factories[kind] = factory
	return nil
}

func (r *CardPlatformRegistry) Create(client *http.Client, config CardPlatformConfig) (ConfiguredCardPlatform, error) {
	if r == nil || client == nil || !cardPlatformKeyPattern.MatchString(config.Key) ||
		!cardPlatformKindPattern.MatchString(config.Kind) {
		return nil, cardFactoryError("CARD_PLATFORM_CONFIGURATION_INVALID")
	}
	r.mu.RLock()
	factory, found := r.factories[config.Kind]
	r.mu.RUnlock()
	if !found {
		return nil, cardFactoryError("CARD_PLATFORM_KIND_UNSUPPORTED")
	}
	if len(config.CredentialJSON) == 0 || len(config.CredentialJSON) > 64<<10 {
		return nil, cardFactoryError("CARD_PLATFORM_NOT_CONFIGURED")
	}
	// A custom constructor cannot mutate the caller's credential buffer. Its
	// private parsing copy is cleared after construction, never retained here.
	config.CredentialJSON = append(json.RawMessage(nil), config.CredentialJSON...)
	defer clear(config.CredentialJSON)
	adapter, err := factory(client, config)
	if err != nil {
		return nil, safeCardFactoryError(err)
	}
	if adapter == nil {
		return nil, cardFactoryError("CARD_PLATFORM_FACTORY_INVALID")
	}
	value := reflect.ValueOf(adapter)
	if value.Kind() == reflect.Pointer && value.IsNil() {
		return nil, cardFactoryError("CARD_PLATFORM_FACTORY_INVALID")
	}
	if adapter.Key() != config.Kind {
		return nil, cardFactoryError("CARD_PLATFORM_FACTORY_INVALID")
	}
	return &configuredCardPlatform{CardPlatform: adapter, key: config.Key, kind: config.Kind}, nil
}

// NewCardPlatform is the worker's default factory. It performs no network I/O.
func NewCardPlatform(client *http.Client, config CardPlatformConfig) (ConfiguredCardPlatform, error) {
	return NewCardPlatformRegistry().Create(client, config)
}

func newConfiguredSpaceX(client *http.Client, config CardPlatformConfig) (CardPlatform, error) {
	var credential struct {
		AppID     string `json:"appId"`
		AppSecret string `json:"appSecret"`
	}
	if json.Unmarshal(config.CredentialJSON, &credential) != nil || strings.TrimSpace(credential.AppSecret) == "" {
		return nil, cardFactoryError("CARD_PLATFORM_NOT_CONFIGURED")
	}
	return NewSpaceXClientWithBaseURL(client, config.BaseURL, credential.AppID, credential.AppSecret)
}

func newConfiguredEfun(client *http.Client, config CardPlatformConfig) (CardPlatform, error) {
	var credential struct {
		APIKey string `json:"apiKey"`
	}
	if json.Unmarshal(config.CredentialJSON, &credential) != nil || strings.TrimSpace(credential.APIKey) == "" {
		return nil, cardFactoryError("CARD_PLATFORM_NOT_CONFIGURED")
	}
	return NewEfunCardClient(client, config.BaseURL, credential.APIKey)
}

func cardFactoryError(code string) error {
	return &Error{ErrorCode: code, Message: code, KnownNoWrite: true}
}

func safeCardFactoryError(err error) error {
	var source *Error
	if errors.As(err, &source) && source != nil {
		switch source.ErrorCode {
		case "CARD_PLATFORM_NOT_CONFIGURED", "CARD_PLATFORM_CONFIGURATION_INVALID",
			"SPACEXCARD_OPENAPI_NOT_CONFIGURED", "SPACEXCARD_CONFIGURATION_INVALID",
			"EFUNCARD_OPENAPI_NOT_CONFIGURED", "EFUNCARD_CONFIGURATION_INVALID":
			return cardFactoryError(source.ErrorCode)
		}
	}
	// A constructor can accidentally include a credential-bearing endpoint or
	// parsing input in its error. Such text must not reach workflow logs.
	return cardFactoryError("CARD_PLATFORM_FACTORY_INVALID")
}
