package providers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/aoagents/agent-orchestrator/backend/internal/process"
)

// Credential names. They name a store entry, a keychain service and an
// environment variable, and carry no secret of their own.
const (
	deepseekKeyName   = "DEEPSEEK_API_KEY"   //nolint:gosec // a credential's name, not a credential value
	openrouterKeyName = "OPENROUTER_API_KEY" //nolint:gosec // a credential's name, not a credential value
	keychainTimeout   = 3 * time.Second
)

// Fixed credential failure reasons. They never carry key material.
var (
	errNoKey      = errors.New("no key")
	errKeyInvalid = errors.New("key has an unusable format")
)

// KeyReader returns a provider's API key for one request. Callers must not
// keep the key beyond that request.
type KeyReader interface {
	Key(ctx context.Context, provider string) (string, error)
}

// Runner runs a helper binary and returns its stdout. Secrets come back on
// stdout only, never through argv.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

func execRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return process.CommandContext(ctx, name, args...).Output()
}

// SystemKeys reads keys from the owner's existing stores:
//   - DeepSeek: the DeepSeek Harness credential store (DSH_CREDENTIALS_PATH,
//     else $DSH_HOME/.credentials.yaml, else ~/.dsh/.credentials.yaml), API-key
//     entry DEEPSEEK_API_KEY only, then the DEEPSEEK_API_KEY environment
//     variable. The Harness login token is never read.
//   - OpenRouter: on macOS the keychain item PAL uses (service
//     OPENROUTER_API_KEY, account $USER) via `security … -w` (secret on stdout),
//     then the OPENROUTER_API_KEY environment variable.
type SystemKeys struct {
	Env  func(string) string // default os.Getenv
	Home func() (string, error)
	Run  Runner // default exec
	GOOS string // default runtime.GOOS
}

func (k SystemKeys) env(name string) string {
	if k.Env != nil {
		return k.Env(name)
	}
	return os.Getenv(name)
}

// Key implements KeyReader.
func (k SystemKeys) Key(ctx context.Context, provider string) (string, error) {
	var key string
	switch provider {
	case DeepSeek:
		key = k.deepseekStored()
		if key == "" {
			key = strings.TrimSpace(k.env(deepseekKeyName))
		}
	case OpenRouter:
		key = k.openrouterKeychain(ctx)
		if key == "" {
			key = strings.TrimSpace(k.env(openrouterKeyName))
		}
	default:
		return "", errUnknownProvider
	}
	if key == "" {
		return "", errNoKey
	}
	if !usableKey(key) {
		return "", errKeyInvalid
	}
	return key, nil
}

// usableKey refuses anything that could break or inject into an HTTP header.
func usableKey(key string) bool {
	if len(key) < 8 || len(key) > 512 {
		return false
	}
	for _, r := range key {
		if r <= ' ' || r > '~' {
			return false
		}
	}
	return true
}

func (k SystemKeys) deepseekPath() string {
	if p := strings.TrimSpace(k.env("DSH_CREDENTIALS_PATH")); p != "" {
		return p
	}
	home := strings.TrimSpace(k.env("DSH_HOME"))
	if home == "" {
		homeDir := k.Home
		if homeDir == nil {
			homeDir = os.UserHomeDir
		}
		h, err := homeDir()
		if err != nil || h == "" {
			return ""
		}
		home = filepath.Join(h, ".dsh")
	}
	return filepath.Join(home, ".credentials.yaml")
}

// deepseekStored reads the DEEPSEEK_API_KEY entry of a version 1 Harness
// store. Any read or parse problem means "no stored key"; the error text is
// never surfaced because a YAML error can quote file content.
func (k SystemKeys) deepseekStored() string {
	path := k.deepseekPath()
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path) //nolint:gosec // the Harness credential store, resolved above
	if err != nil || len(data) > 1<<20 {
		return ""
	}
	var store map[string]any
	if yaml.Unmarshal(data, &store) != nil || !storeVersion1(store) {
		return ""
	}
	return findEntry(store, deepseekKeyName, 0)
}

func storeVersion1(store map[string]any) bool {
	switch v := store["version"].(type) {
	case int:
		return v == 1
	case float64:
		return v == 1
	case string:
		return strings.TrimSpace(v) == "1"
	}
	return false
}

// findEntry walks the store for the entry named key and returns its secret:
// the scalar form or the nested {payload: {secret: …}} form.
func findEntry(value any, key string, depth int) string {
	if depth > 8 {
		return ""
	}
	switch typed := value.(type) {
	case map[string]any:
		if child, ok := typed[key]; ok {
			if s := secretOf(child, 0); s != "" {
				return s
			}
		}
		for _, child := range typed {
			if s := findEntry(child, key, depth+1); s != "" {
				return s
			}
		}
	case []any:
		for _, child := range typed {
			if s := findEntry(child, key, depth+1); s != "" {
				return s
			}
		}
	}
	return ""
}

func secretOf(value any, depth int) string {
	if depth > 4 {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case map[string]any:
		for _, field := range []string{"secret", "value", "key", "token", "payload"} {
			if child, ok := typed[field]; ok {
				if s := secretOf(child, depth+1); s != "" {
					return s
				}
			}
		}
	}
	return ""
}

var keychainAccountPattern = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// openrouterKeychain reads the keychain item PAL's MCP wrapper uses. The
// secret arrives on the helper's stdout; argv carries only the service and
// account names. Any failure (missing item, locked keychain, timeout) falls
// back to the environment.
func (k SystemKeys) openrouterKeychain(ctx context.Context) string {
	goos := k.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	if goos != "darwin" {
		return ""
	}
	account := k.env("USER")
	if account == "" {
		if u, err := user.Current(); err == nil {
			account = u.Username
		}
	}
	if !keychainAccountPattern.MatchString(account) {
		return ""
	}
	run := k.Run
	if run == nil {
		run = execRunner
	}
	ctx, cancel := context.WithTimeout(ctx, keychainTimeout)
	defer cancel()
	out, err := run(ctx, "security", "find-generic-password", "-s", openrouterKeyName, "-a", account, "-w")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// KeyID is the first 12 hex characters of SHA-256(key): a one-way identifier
// used to tell keys apart and to de-duplicate a key shared by several hosts.
// The PAL ledger uses the same algorithm.
func KeyID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:12]
}
