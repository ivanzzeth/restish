// Browser-backed HTTP transport.
//
// Restish has no per-request hook/rewrite mechanism, and a bare net/http
// client is often blocked by anti-bot layers. This round tripper instead
// forwards each request over loopback to a local browser-backed forwarder
// (open-surface's `debug forward`), which issues the request from a real
// browser page context — carrying the browser's TLS fingerprint, cookies, and
// login state. Any site a browser can reach can therefore be requested by the
// generated CLI.
//
// The forwarder process is spawned lazily on the first request and killed when
// the transport is closed. The protocol is JSON over loopback:
//
//	POST /request  -> {"method","url","headers","body"(base64),"target"}
//	                 {"status","status_text","headers","body"(base64),...}
//	GET  /healthz  -> 200 {"ok": true}

package request

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// BrowserRoundTripperConfig controls the browser-backed transport.
type BrowserRoundTripperConfig struct {
	// Target is the open-surface artifact (session) whose browser profile
	// serves requests, e.g. "xiaohongshu".
	Target string
	// Port pins the forwarder port; 0 picks a free port.
	Port int
	// Logger receives diagnostic output; may be nil.
	Logger io.Writer
}

// BrowserRoundTripper forwards requests through a browser-backed forwarder.
type BrowserRoundTripper struct {
	cfg BrowserRoundTripperConfig

	once     sync.Once
	port     int
	cmd      *exec.Cmd
	done     chan error
	exitErr  error
	reaped   bool
	err      error
	spawnErr error
	stderr   *tailBuffer
}

// NewBrowserRoundTripper builds a browser-backed transport. It implements
// http.RoundTripper and may optionally be closed to reap the forwarder.
func NewBrowserRoundTripper(cfg BrowserRoundTripperConfig) *BrowserRoundTripper {
	return &BrowserRoundTripper{cfg: cfg}
}

var _ http.RoundTripper = (*BrowserRoundTripper)(nil)

type forwardResponse struct {
	Status     int               `json:"status"`
	StatusText string            `json:"status_text"`
	Headers    map[string]string `json:"headers"`
	Body       string            `json:"body"`
	URL        string            `json:"url"`
	Via        string            `json:"via"`
	Challenge  bool              `json:"challenge"`
}

// RoundTrip serializes the request, sends it to the forwarder, and rebuilds
// the response from the browser's actual reply. Status codes are passed
// through untouched (406 etc. are not swallowed).
func (t *BrowserRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	t.once.Do(t.start)
	if t.err != nil {
		return nil, t.err
	}

	payload, err := t.serialize(req)
	if err != nil {
		return nil, fmt.Errorf("browser forwarder: serializing request: %w", err)
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("browser forwarder: encoding request: %w", err)
	}

	fwdURL := fmt.Sprintf("http://127.0.0.1:%d/request", t.port)
	// The loopback hop is part of the original request, not an independent
	// fifteen-minute operation.  Inheriting the caller context makes
	// --rsh-timeout and MCP request cancellation stop the forwarder request too.
	fwdReq, err := http.NewRequestWithContext(
		req.Context(), http.MethodPost, fwdURL, bytes.NewReader(data),
	)
	if err != nil {
		return nil, fmt.Errorf("browser forwarder: %w", err)
	}
	fwdReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	fwdResp, err := client.Do(fwdReq)
	if err != nil {
		return nil, fmt.Errorf("browser forwarder: %w", err)
	}
	defer fwdResp.Body.Close()

	if fwdResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(fwdResp.Body)
		return nil, fmt.Errorf(
			"browser forwarder error (HTTP %d): %s",
			fwdResp.StatusCode,
			strings.TrimSpace(string(body)),
		)
	}

	var out forwardResponse
	if err := json.NewDecoder(fwdResp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("browser forwarder: decoding response: %w", err)
	}
	body, err := base64.StdEncoding.DecodeString(out.Body)
	if err != nil {
		return nil, fmt.Errorf("browser forwarder: decoding body: %w", err)
	}

	// The forwarder returns an already-decoded body (the browser's fetch
	// decompressed it), so any Content-Encoding / Content-Length describing the
	// original wire form must not survive: downstream would try to gunzip
	// plaintext or truncate to the compressed length.
	hdr := make(http.Header, len(out.Headers))
	for k, v := range out.Headers {
		if isBodyEncodingHeader(k) {
			continue
		}
		hdr.Set(k, v)
	}
	hdr.Set("Content-Length", strconv.Itoa(len(body)))
	statusText := out.StatusText
	if statusText == "" {
		statusText = http.StatusText(out.Status)
	}
	if statusText == "" {
		statusText = "Browser Response"
	}

	return &http.Response{
		Status:        fmt.Sprintf("%d %s", out.Status, statusText),
		StatusCode:    out.Status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        hdr,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}, nil
}

// isBodyEncodingHeader reports whether a header describes the body's on-the-wire
// encoding rather than the decoded payload the forwarder hands back.
func isBodyEncodingHeader(name string) bool {
	switch strings.ToLower(name) {
	case "content-encoding", "content-length", "transfer-encoding":
		return true
	}
	return false
}

// Close reaps the forwarder subprocess, if any was started.
func (t *BrowserRoundTripper) Close() error {
	if t.cmd == nil || t.cmd.Process == nil {
		return nil
	}
	err := t.cmd.Process.Kill()
	t.waitProcess()
	t.cmd = nil
	return err
}

// Probe starts the exact forwarder subprocess used by RoundTrip, waits for
// /healthz, and immediately reaps it.  It never sends an upstream request.
// Generated-runtime doctors use this to catch import, bind and lifecycle
// failures before the first authorized platform call.
func (t *BrowserRoundTripper) Probe() error {
	t.once.Do(t.start)
	if t.err != nil {
		return t.err
	}
	return t.Close()
}

// CloseIdleConnections is a no-op kept for http.Transport interface parity.
func (t *BrowserRoundTripper) CloseIdleConnections() {}

func (t *BrowserRoundTripper) serialize(req *http.Request) (map[string]any, error) {
	headers := make(map[string]string, len(req.Header))
	for k, vs := range req.Header {
		headers[k] = strings.Join(vs, ", ")
	}
	payload := map[string]any{
		"method":  req.Method,
		"url":     req.URL.String(),
		"headers": headers,
		"target":  t.cfg.Target,
	}
	if req.Body != nil {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		if len(body) > 0 {
			payload["body"] = base64.StdEncoding.EncodeToString(body)
		}
		_ = req.Body.Close()
	}
	return payload, nil
}

// start spawns the forwarder and waits until it is ready. Called once.
func (t *BrowserRoundTripper) start() {
	if t.cfg.Port != 0 {
		t.port = t.cfg.Port
		if t.spawnAndWait() {
			return
		}
		t.kill()
		t.err = forwarderError(t.cfg.Port, t.spawnErr, t.stderr.String())
		return
	}
	// Pick a free port, retrying once if the forwarder fails to come up.
	for attempt := 0; attempt < 2; attempt++ {
		t.port = randomPort()
		if t.spawnAndWait() {
			return
		}
		t.kill()
	}
	t.err = forwarderError(t.port, t.spawnErr, t.stderr.String())
}

func (t *BrowserRoundTripper) spawnAndWait() bool {
	if err := t.spawn(t.port); err != nil {
		t.spawnErr = err
		return false
	}
	t.spawnErr = nil
	return t.waitReady()
}

func (t *BrowserRoundTripper) spawn(port int) error {
	pythonBin, err := resolvePython()
	if err != nil {
		return err
	}
	args := []string{
		"-m", "open_surface.reverse.forward",
		"--target", t.cfg.Target,
		"--port", strconv.Itoa(port),
	}
	cmd := exec.Command(pythonBin, args...)
	// Tee the forwarder's stderr: it still streams to the user, and the tail is
	// retained so a failed startup reports the real cause (a traceback, a
	// missing browser) instead of a generic "did not become ready".
	if t.stderr == nil {
		t.stderr = newTailBuffer(4096)
	}
	t.stderr.Reset()
	diagnostics := io.MultiWriter(os.Stderr, t.stderr)
	cmd.Stderr = diagnostics
	// stdout may be an MCP JSON-RPC/stdio transport. The Python forwarder and
	// browser runtime are allowed to emit startup diagnostics (including ANSI),
	// but those bytes must never corrupt the protocol stream.
	cmd.Stdout = diagnostics
	if err := cmd.Start(); err != nil {
		return err
	}
	t.cmd = cmd
	t.done = make(chan error, 1)
	t.reaped = false
	go func() {
		t.done <- cmd.Wait()
		close(t.done)
	}()
	return nil
}

// resolvePython finds an interpreter that can actually import open_surface.
// A bare PATH lookup is not enough: open-surface is commonly installed only in
// a project virtualenv, so `python3` resolves to a system interpreter without
// the module and the forwarder dies on import.
func resolvePython() (string, error) {
	if bin := os.Getenv("OPEN_SURFACE_PYTHON"); bin != "" {
		// Explicit choice wins, even if the import probe fails — the user gets
		// the interpreter's own error rather than a silent substitution.
		return bin, nil
	}

	var tried []string
	for _, cand := range pythonCandidates() {
		if cand == "" {
			continue
		}
		bin, err := exec.LookPath(cand)
		if err != nil {
			continue
		}
		tried = append(tried, bin)
		if exec.Command(bin, "-c", "import open_surface").Run() == nil {
			return bin, nil
		}
	}
	if len(tried) == 0 {
		return "", fmt.Errorf("python3 not found in PATH (set OPEN_SURFACE_PYTHON)")
	}
	return "", fmt.Errorf(
		"no Python interpreter with open-surface installed (tried %s); "+
			`install it (pip install "open-surface[reverse]") or point `+
			"OPEN_SURFACE_PYTHON at the right interpreter",
		strings.Join(tried, ", "),
	)
}

// pythonCandidates lists interpreters to probe, most specific first: virtualenvs
// near the working directory and the open-surface checkout, then PATH.
func pythonCandidates() []string {
	var cands []string
	addVenv := func(dir string) {
		if dir != "" {
			cands = append(cands, filepath.Join(dir, ".venv", "bin", "python"))
		}
	}
	addVenv(os.Getenv("OPEN_SURFACE_ROOT"))
	if venv := os.Getenv("VIRTUAL_ENV"); venv != "" {
		cands = append(cands, filepath.Join(venv, "bin", "python"))
	}
	// Walk up from the working directory: `./call` runs inside the artifact
	// tree, whose repo root usually holds the virtualenv.
	if wd, err := os.Getwd(); err == nil {
		for dir := wd; ; {
			addVenv(dir)
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return append(cands, "python3", "python")
}

// tailBuffer keeps the last n bytes written to it, discarding older output.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func newTailBuffer(max int) *tailBuffer { return &tailBuffer{max: max} }

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.max {
		b.buf = b.buf[len(b.buf)-b.max:]
	}
	return len(p), nil
}

func (b *tailBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = nil
}

func (b *tailBuffer) String() string {
	if b == nil {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(string(b.buf))
}

func (t *BrowserRoundTripper) waitReady() bool {
	deadline := time.Now().Add(30 * time.Second)
	healthURL := fmt.Sprintf("http://127.0.0.1:%d/healthz", t.port)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		if t.processExited() {
			return false
		}
		resp, err := client.Get(healthURL)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}

func (t *BrowserRoundTripper) processExited() bool {
	if t.cmd == nil || t.cmd.Process == nil || t.done == nil {
		return true
	}
	select {
	case t.exitErr = <-t.done:
		t.reaped = true
		return true
	default:
		return false
	}
}

func (t *BrowserRoundTripper) kill() {
	if t.cmd != nil && t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
		t.waitProcess()
		t.cmd = nil
	}
}

func (t *BrowserRoundTripper) waitProcess() {
	if t.reaped || t.done == nil {
		return
	}
	t.exitErr = <-t.done
	t.reaped = true
}

func forwarderError(port int, spawnErr error, stderr string) error {
	// A spawn failure (e.g. no interpreter with open-surface) never started a
	// process, so it carries the precise cause — surface it directly.
	if spawnErr != nil {
		return fmt.Errorf("browser forwarder could not start: %w", spawnErr)
	}
	err := fmt.Errorf(
		"browser forwarder on 127.0.0.1:%d did not become ready; "+
			"install the browser transport deps and retry: "+
			`pip install "open-surface[reverse]" && playwright install chromium`,
		port,
	)
	if stderr == "" {
		return err
	}
	return fmt.Errorf("%w\nforwarder output:\n%s", err, stderr)
}

func randomPort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 40000 + rand.Intn(10000)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
