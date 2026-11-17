package cli

// Legacy bootstrap helpers remain only to preserve their existing unit coverage.
// Local-distribution production binaries contain no release acquisition path.
import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
)

var releaseRepo = "Untrivial-ai/agent-orchestrator"

// download streams url to dst using the injected HTTP client, reporting progress
// to w. asset is the human-facing filename used in the announce line. A silent
// hundreds-of-MB fetch reads as a hang, so we print a start line (with the size
// from Content-Length when present) and then progress: a live carriage-return
// percentage when w is an interactive terminal, or a plain start+done pair when
// it is not (CI, pipes, redirected output).
func (c *commandContext) download(ctx context.Context, w io.Writer, url, asset, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return err
	}
	// deps.HTTPClient carries a short (2s) timeout sized for loopback daemon
	// probes; a release asset is hundreds of MB. Copy the client and drop the
	// timeout, relying on ctx for cancellation. The Transport is preserved so
	// tests still reach their httptest server.
	client := *c.deps.HTTPClient
	client.Timeout = 0
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %s", resp.Status)
	}

	// Content-Length is -1 when the server omits the header; total<=0 means the
	// percentage is unknown and we report transferred bytes instead.
	total := resp.ContentLength
	if total > 0 {
		_, _ = fmt.Fprintf(w, "Downloading Agent Orchestrator (%s, ~%s) from %s...\n", asset, humanBytes(total), releaseRepo)
	} else {
		_, _ = fmt.Fprintf(w, "Downloading Agent Orchestrator (%s) from %s...\n", asset, releaseRepo)
	}

	f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	pw := &progressWriter{w: w, total: total, tty: writerIsInteractive(w)}
	if _, err := io.Copy(f, io.TeeReader(resp.Body, pw)); err != nil {
		return err
	}
	pw.done()
	return f.Close()
}

// progressWriter counts bytes copied through it and renders download progress to
// w. On an interactive terminal it overwrites a single line with a carriage
// return; otherwise it stays quiet during the copy and prints one "Done" line at
// the end, so logs and pipes never fill with \r spam or per-chunk noise.
type progressWriter struct {
	w       io.Writer
	total   int64 // bytes; <=0 when Content-Length was absent
	tty     bool
	written int64
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n := len(b)
	p.written += int64(n)
	if p.tty {
		if p.total > 0 {
			pct := p.written * 100 / p.total
			_, _ = fmt.Fprintf(p.w, "\r  %d%% (%s / %s)", pct, humanBytes(p.written), humanBytes(p.total))
		} else {
			_, _ = fmt.Fprintf(p.w, "\r  %s", humanBytes(p.written))
		}
	}
	return n, nil
}

// done emits the terminal progress line. On a TTY it closes the live line with a
// newline; off a TTY it prints the single completion line.
func (p *progressWriter) done() {
	if p.tty {
		_, _ = fmt.Fprintln(p.w)
		return
	}
	_, _ = fmt.Fprintf(p.w, "Downloaded %s.\n", humanBytes(p.written))
}

// writerIsInteractive reports whether w is an interactive terminal, mirroring
// stdinIsInteractive: only a real *os.File backed by a character device counts,
// so a bytes.Buffer or pipe is treated as non-interactive.
func writerIsInteractive(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// humanBytes formats a byte count with a binary-unit suffix (KiB, MiB, ...),
// e.g. 314572800 -> "300.0 MiB". A tiny hand-rolled formatter avoids promoting
// the already-indirect go-humanize dependency to a direct one for one string.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// assetName maps the current GOOS/GOARCH to the stable release asset name the
// release pipeline publishes (spec §6.3, §8). The pipeline uses "x64" for amd64.
//   - darwin: agent-orchestrator-darwin-{arm64,x64}.zip (signed bundle zip)
//   - windows: agent-orchestrator-win32-x64.exe (NSIS installer, amd64 only)
//   - linux: agent-orchestrator-linux-x64.AppImage (self-contained, amd64 only)
func assetName() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		arch, err := assetArch(runtime.GOARCH)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("agent-orchestrator-darwin-%s.zip", arch), nil
	case "windows":
		if _, err := requireAMD64(); err != nil {
			return "", err
		}
		return "agent-orchestrator-win32-x64.exe", nil
	case "linux":
		if _, err := requireAMD64(); err != nil {
			return "", err
		}
		return "agent-orchestrator-linux-x64.AppImage", nil
	default:
		return "", fmt.Errorf("ao start: no release asset for %s", runtime.GOOS)
	}
}

// requireAMD64 enforces the amd64/x64-only support window for Windows and Linux.
// arm64 Windows/Linux are not published yet; surface a clear unsupported error
// mirroring assetArch rather than fetching a 404.
func requireAMD64() (string, error) {
	if runtime.GOARCH != "amd64" {
		return "", fmt.Errorf("ao start: unsupported architecture %q on %s (only amd64 is published)", runtime.GOARCH, runtime.GOOS)
	}
	return "x64", nil
}

// assetArch maps a Go GOARCH to the release-asset arch token.
func assetArch(goarch string) (string, error) {
	switch goarch {
	case "arm64":
		return "arm64", nil
	case "amd64":
		return "x64", nil
	default:
		return "", fmt.Errorf("ao start: unsupported architecture %q", goarch)
	}
}

// downloadURL builds the constant releases/latest/download URL for asset.
func downloadURL(asset string) string {
	return fmt.Sprintf("https://github.com/%s/releases/latest/download/%s", releaseRepo, asset)
}
