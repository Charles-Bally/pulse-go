package pulse

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	defaultEndpoint    = "https://api.pulse-app.si"
	requestTimeout     = 10 * time.Second
	maxAppVersionChars = 60
)

var (
	keyPattern = regexp.MustCompile(`^pk_(live|test)_[A-Za-z0-9_-]{20,}$`)
	localHosts = map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}
)

type lookupFunc func(string) (string, bool)

type config struct {
	key        string
	endpoint   string
	appVersion string
	publicKey  []byte
	clear      []string
	selection  selection
	logger     *slog.Logger
	client     *http.Client
}

type selfKey struct{}

// Les messages de Pulse portent component=pulse : la capture des journaux les reconnaît et ne les renvoie pas.
func say(logger *slog.Logger, level slog.Level, message string) {
	if logger == nil {
		logger = slog.Default()
	}
	ctx := context.WithValue(context.Background(), selfKey{}, true)
	logger.LogAttrs(ctx, level, message, slog.String("component", "pulse"))
}

func envValue(env lookupFunc, name string) string {
	value, _ := env(name)
	return strings.TrimSpace(value)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// La clé voyage dans un en-tête : jamais en clair hors de la machine, sauf vers un Pulse local.
func safeEndpoint(raw string) (string, bool) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return "", false
	}
	local := parsed.Scheme == "http" && localHosts[parsed.Hostname()]
	if parsed.Scheme != "https" && !local {
		return "", false
	}
	return parsed.Scheme + "://" + parsed.Host, true
}

func resolveConfig(opts Options, env lookupFunc) *config {
	logger := opts.Logger
	if opts.Disabled || envValue(env, "PULSE_DISABLED") == "true" {
		return nil
	}
	key := firstNonEmpty(opts.Key, envValue(env, "PULSE_KEY"))
	if key == "" {
		say(logger, slog.LevelWarn, "PULSE_KEY absente : Pulse ne surveille pas cette app.")
		return nil
	}
	if !keyPattern.MatchString(key) {
		say(logger, slog.LevelError, "PULSE_KEY mal formée : elle commence par pk_live_ ou pk_test_.")
		return nil
	}
	endpoint, ok := safeEndpoint(firstNonEmpty(opts.Endpoint, envValue(env, "PULSE_ENDPOINT"), defaultEndpoint))
	if !ok {
		say(logger, slog.LevelError, "PULSE_ENDPOINT doit être en https.")
		return nil
	}
	rawPublicKey := firstNonEmpty(opts.PublicKey, envValue(env, "PULSE_PUBLIC_KEY"))
	publicKey := parsePublicKey(rawPublicKey)
	if rawPublicKey != "" && publicKey == nil {
		say(logger, slog.LevelError, "PULSE_PUBLIC_KEY mal formée : copie-la depuis l’app Pulse, elle commence par pulse_pub_.")
		return nil
	}
	rawFingerprint := firstNonEmpty(opts.RelayFingerprint, envValue(env, "PULSE_RELAY_FINGERPRINT"))
	fingerprint := normalizeFingerprint(rawFingerprint)
	if rawFingerprint != "" && fingerprint == "" {
		say(logger, slog.LevelError, "PULSE_RELAY_FINGERPRINT mal formée : copie l’empreinte SHA-256 affichée par ton relais.")
		return nil
	}
	cfg := &config{
		key:        key,
		endpoint:   endpoint,
		appVersion: truncate(firstNonEmpty(opts.AppVersion, envValue(env, "PULSE_APP_VERSION"), buildVersion()), maxAppVersionChars),
		publicKey:  publicKey,
		selection:  resolveSelection(opts, env),
		logger:     logger,
		client:     opts.HTTPClient,
	}
	if publicKey != nil {
		cfg.clear = clearSelection(opts.Clear, env)
	}
	if cfg.client == nil {
		cfg.client = newClient(fingerprint)
	}
	return cfg
}

func clearSelection(option []string, env lookupFunc) []string {
	if option != nil {
		return canonical(clearFields, option)
	}
	raw, ok := env("PULSE_CLEAR")
	if !ok {
		return []string{"status"}
	}
	return canonical(clearFields, splitList(raw))
}

func (c *config) warn(message string) { say(c.logger, slog.LevelWarn, message) }

func (c *config) fail(message string) { say(c.logger, slog.LevelError, message) }
