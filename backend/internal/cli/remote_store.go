package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
)

// remoteKeychainService is the macOS keychain service holding each saved
// remote's device token; the account is the remote's name.
const remoteKeychainService = "icosa-orchestrator-cli"

// cliRemote is a saved remote AO host. It holds no secret: the device token
// lives in the remoteSecretStore under the same name.
type cliRemote struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	HostID   string `json:"hostId"`
	DeviceID string `json:"deviceId,omitempty"`
	// AllowInsecure records that the user explicitly allowed sending the token
	// over plain http to an address that is neither loopback nor Tailscale.
	AllowInsecure bool      `json:"allowInsecure,omitempty"`
	AddedAt       time.Time `json:"addedAt"`
}

type cliRemotesFile struct {
	Remotes []cliRemote `json:"remotes"`
}

var remoteNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// deviceTokenPattern bounds what the CLI ever stores or sends: the host issues
// opaque base64url-style tokens, and the keychain write quotes the value.
var deviceTokenPattern = regexp.MustCompile(`^[A-Za-z0-9_.\-]{16,512}$`)

func validRemoteName(name string) error {
	if !remoteNamePattern.MatchString(name) {
		return usageError{fmt.Errorf("invalid remote name %q: use letters, digits, '.', '_' or '-' (max 63)", name)}
	}
	return nil
}

// remotesDir is the daemon's data dir, resolved as the CLI resolves the
// daemon ($AO_DATA_DIR, else the host launch profile, else ~/.ao/dev/data).
// The user's terminal and the agents that daemon spawns (which get
// AO_DATA_DIR set to it) then share one file, and isolated runs stay
// isolated. Tests may point Deps.RemotesDir elsewhere.
func (c *commandContext) remotesDir() (string, error) {
	if c.deps.RemotesDir != "" {
		return c.deps.RemotesDir, nil
	}
	_, dataDir, _, err := config.ResolvePaths()
	if err != nil {
		return "", err
	}
	return dataDir, nil
}

func (c *commandContext) remotesFilePath() (string, error) {
	dir, err := c.remotesDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "cli-remotes.json"), nil
}

func (c *commandContext) loadRemotes() ([]cliRemote, error) {
	path, err := c.remotesFilePath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path) // #nosec G304 -- fixed file under ~/.ao.
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var file cliRemotesFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	sort.Slice(file.Remotes, func(i, j int) bool { return file.Remotes[i].Name < file.Remotes[j].Name })
	return file.Remotes, nil
}

func (c *commandContext) saveRemotes(remotes []cliRemote) error {
	path, err := c.remotesFilePath()
	if err != nil {
		return err
	}
	sort.Slice(remotes, func(i, j int) bool { return remotes[i].Name < remotes[j].Name })
	data, err := json.MarshalIndent(cliRemotesFile{Remotes: remotes}, "", "  ")
	if err != nil {
		return err
	}
	return writePrivateFile(path, append(data, '\n'))
}

func (c *commandContext) findRemote(name string) (cliRemote, bool, error) {
	remotes, err := c.loadRemotes()
	if err != nil {
		return cliRemote{}, false, err
	}
	for _, r := range remotes {
		if r.Name == name {
			return r, true, nil
		}
	}
	return cliRemote{}, false, nil
}

// writePrivateFile replaces path atomically with a 0600 file in a 0700 dir.
func writePrivateFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// remoteSecretStore keeps each remote's device token out of plain config.
type remoteSecretStore interface {
	Get(ctx context.Context, name string) (string, error)
	Set(ctx context.Context, name, token string) error
	Delete(ctx context.Context, name string) error
	// Describe names where tokens live, for user-facing messages.
	Describe() string
}

var errRemoteTokenMissing = errors.New("no saved token")

func (c *commandContext) remoteSecrets() (remoteSecretStore, error) {
	if c.deps.RemoteSecrets != nil {
		return c.deps.RemoteSecrets, nil
	}
	dir, err := c.remotesDir()
	if err != nil {
		return nil, err
	}
	if runtime.GOOS == "darwin" {
		if path, err := c.deps.LookPath("security"); err == nil {
			return newKeychainSecretStore(path, dir), nil
		}
	}
	return fileSecretStore{path: filepath.Join(dir, "cli-remote-tokens.json")}, nil
}

// keychainSecretStore uses the macOS login keychain through security(1). The
// write goes through `security -i` on stdin so the token never appears in a
// process argument list. That mode exits 0 even when a command fails, so
// every write is read back before it counts.
type keychainSecretStore struct {
	security string
	// scope ties each item to one data dir: the keychain is shared by every
	// data dir, so a scratch AO_DATA_DIR run must not overwrite the real
	// remote's token under the same name.
	scope string
	// run executes security with stdin; nil runs the real binary.
	run func(ctx context.Context, stdin string, args ...string) ([]byte, int, error)
}

// newKeychainSecretStore scopes items to the remotes dir that holds their
// metadata.
func newKeychainSecretStore(security, remotesDir string) keychainSecretStore {
	sum := sha256.Sum256([]byte(filepath.Clean(remotesDir)))
	return keychainSecretStore{security: security, scope: hex.EncodeToString(sum[:])[:12]}
}

// account is the keychain account for a remote: <name>@<data dir hash>.
func (k keychainSecretStore) account(name string) string {
	if k.scope == "" {
		return name
	}
	return name + "@" + k.scope
}

func (k keychainSecretStore) exec(ctx context.Context, stdin string, args ...string) ([]byte, int, error) {
	if k.run != nil {
		return k.run(ctx, stdin, args...)
	}
	cmd := aoprocess.CommandContext(ctx, k.security, args...) // #nosec G204 G702 -- fixed security(1) binary and fixed verbs.
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.Output()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return out, exitErr.ExitCode(), nil
	}
	return out, 0, err
}

func (k keychainSecretStore) Describe() string {
	return "the login keychain (service " + remoteKeychainService + ", account " + k.account("<name>") + ")"
}

func (k keychainSecretStore) Get(ctx context.Context, name string) (string, error) {
	out, code, err := k.exec(ctx, "", "find-generic-password", "-s", remoteKeychainService, "-a", k.account(name), "-w")
	if err != nil {
		return "", fmt.Errorf("read keychain: %w", err)
	}
	if code == 44 {
		return "", errRemoteTokenMissing
	}
	if code != 0 {
		return "", fmt.Errorf("read keychain: security exited %d", code)
	}
	return strings.TrimSpace(string(out)), nil
}

func (k keychainSecretStore) Set(ctx context.Context, name, token string) error {
	if !deviceTokenPattern.MatchString(token) || !remoteNamePattern.MatchString(name) {
		return errors.New("refusing to store a malformed token or name")
	}
	line := fmt.Sprintf("add-generic-password -U -s %q -a %q -l %q -w %q\n", remoteKeychainService, k.account(name), "Orchestrator CLI remote "+name, token)
	if _, _, err := k.exec(ctx, line, "-i"); err != nil {
		return fmt.Errorf("write keychain: %w", err)
	}
	got, err := k.Get(ctx, name)
	if err != nil {
		return fmt.Errorf("write keychain: %w", err)
	}
	if got != token {
		return errors.New("write keychain: the saved token did not read back; is the login keychain unlocked?")
	}
	return nil
}

func (k keychainSecretStore) Delete(ctx context.Context, name string) error {
	_, code, err := k.exec(ctx, "", "delete-generic-password", "-s", remoteKeychainService, "-a", k.account(name))
	if err != nil {
		return fmt.Errorf("delete keychain item: %w", err)
	}
	if code != 0 && code != 44 {
		return fmt.Errorf("delete keychain item: security exited %d", code)
	}
	return nil
}

// fileSecretStore keeps tokens in one 0600 JSON file under ~/.ao, for hosts
// without a keychain the CLI can drive. Windows has no POSIX mode bits (Go
// reports every writable file as 0666), so there the file relies on the
// user-profile directory's ACL and the mode check is skipped; DPAPI or the
// Credential Manager would be the stronger store there.
type fileSecretStore struct{ path string }

// fileModesEnforced reports whether 0600/0700 modes mean anything here.
var fileModesEnforced = runtime.GOOS != "windows"

func (f fileSecretStore) Describe() string {
	if !fileModesEnforced {
		return f.path + " (user profile)"
	}
	return f.path + " (0600)"
}

func (f fileSecretStore) load() (map[string]string, error) {
	data, err := os.ReadFile(f.path) // #nosec G304 -- fixed file under ~/.ao.
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(f.path); err == nil && fileModesEnforced && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is readable by other users; run chmod 600 on it", f.path)
	}
	tokens := map[string]string{}
	if err := json.Unmarshal(data, &tokens); err != nil {
		return nil, fmt.Errorf("parse %s: %w", f.path, err)
	}
	return tokens, nil
}

func (f fileSecretStore) save(tokens map[string]string) error {
	data, err := json.MarshalIndent(tokens, "", "  ")
	if err != nil {
		return err
	}
	return writePrivateFile(f.path, append(data, '\n'))
}

func (f fileSecretStore) Get(_ context.Context, name string) (string, error) {
	tokens, err := f.load()
	if err != nil {
		return "", err
	}
	token, ok := tokens[name]
	if !ok {
		return "", errRemoteTokenMissing
	}
	return token, nil
}

func (f fileSecretStore) Set(_ context.Context, name, token string) error {
	tokens, err := f.load()
	if err != nil {
		return err
	}
	tokens[name] = token
	return f.save(tokens)
}

func (f fileSecretStore) Delete(_ context.Context, name string) error {
	tokens, err := f.load()
	if err != nil {
		return err
	}
	if _, ok := tokens[name]; !ok {
		return nil
	}
	delete(tokens, name)
	return f.save(tokens)
}
