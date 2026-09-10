// Tests for the browser-backed transport protocol (serialization / response
// assembly). The forwarder subprocess is NOT spawned here: the round tripper
// is pre-marked ready pointing at a local httptest server that plays the
// forwarder role, so the Go<->JSON protocol is exercised without a browser.

package request

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestForwarderErrorPreservesLoopbackBindPermissionClassification(t *testing.T) {
	tests := []struct {
		name   string
		stderr string
	}{
		{
			name: "python permission error eperm",
			stderr: "Traceback (most recent call last):\n" +
				"  File \"socketserver.py\", line 472, in server_bind\n" +
				"    self.socket.bind(self.server_address)\n" +
				"PermissionError: [Errno 1] Operation not permitted\n",
		},
		{
			name: "python os error eacces",
			stderr: "  File \"socketserver.py\", line 472, in server_bind\n" +
				"OSError: [Errno 13] Permission denied\n",
		},
		{
			name:   "symbolic errno",
			stderr: "bind: EPERM\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := forwarderError(43123, nil, tt.stderr)
			if !errors.Is(err, os.ErrPermission) {
				t.Fatalf("bind denial lost permission classification: %v", err)
			}
			if !strings.Contains(err.Error(), tt.stderr) {
				t.Fatalf("bind denial lost original forwarder output: %v", err)
			}
			if strings.Contains(err.Error(), "playwright install chromium") {
				t.Fatalf("bind denial was misdiagnosed as missing browser runtime: %v", err)
			}
		})
	}
}

func TestForwarderErrorKeepsBrowserDependencyHintForUnclassifiedStartupFailure(t *testing.T) {
	err := forwarderError(43123, nil, "ModuleNotFoundError: No module named 'playwright'\n")
	if errors.Is(err, os.ErrPermission) {
		t.Fatalf("missing dependency was misclassified as permission denial: %v", err)
	}
	if !strings.Contains(err.Error(), "playwright install chromium") {
		t.Fatalf("missing dependency lost its setup hint: %v", err)
	}
}

func TestBrowserRoundTripperHonorsCallerCancellation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	fwd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
	}))
	defer func() {
		close(release)
		fwd.Close()
	}()

	port := strings.TrimPrefix(fwd.URL, "http://127.0.0.1:")
	rt := NewBrowserRoundTripper(BrowserRoundTripperConfig{Target: "mysite"})
	rt.ready(atoiSafe(port))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.example.com/x", nil)

	begin := time.Now()
	_, err := rt.RoundTrip(req)
	if err == nil {
		t.Fatal("expected request cancellation")
	}
	if elapsed := time.Since(begin); elapsed > time.Second {
		t.Fatalf("cancellation took %s; loopback request ignored caller context", elapsed)
	}
	select {
	case <-started:
	default:
		t.Fatal("fixture forwarder never received request")
	}
}

func TestBrowserRoundTripperCancelsStartupAndReapsProcess(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("shell fixture is POSIX-only")
	}
	launcher := filepath.Join(t.TempDir(), "python")
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\nexec sleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPEN_SURFACE_PYTHON", launcher)
	rt := NewBrowserRoundTripper(BrowserRoundTripperConfig{Target: "mysite"})
	defer rt.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.example.com/x", nil)
	begin := time.Now()
	_, err := rt.RoundTrip(req)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("startup lost caller deadline: %v", err)
	}
	if elapsed := time.Since(begin); elapsed > time.Second {
		t.Fatalf("startup cancellation took %s", elapsed)
	}
	if rt.cmd != nil || !rt.reaped {
		t.Fatal("canceled startup left its subprocess unreaped")
	}
}

func TestBrowserRoundTripperDetectsExitedForwarderWithoutReadinessTimeout(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("shell fixture is POSIX-only")
	}
	dir := t.TempDir()
	python := filepath.Join(dir, "python")
	if err := os.WriteFile(
		python,
		[]byte("#!/bin/sh\nprintf '\\033[31mforwarder-stdout\\033[0m\\n'\necho startup-stderr >&2\nexit 23\n"),
		0o755,
	); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPEN_SURFACE_PYTHON", python)
	rt := NewBrowserRoundTripper(BrowserRoundTripperConfig{Target: "mysite"})

	begin := time.Now()
	err := rt.Probe()
	if err == nil {
		t.Fatal("expected readiness failure")
	}
	if elapsed := time.Since(begin); elapsed > 3*time.Second {
		t.Fatalf("dead forwarder took %s to detect", elapsed)
	}
	if !strings.Contains(err.Error(), "forwarder-stdout") || !strings.Contains(err.Error(), "startup-stderr") {
		t.Fatalf("startup diagnostics were not isolated and preserved: %v", err)
	}
}

func TestBrowserRoundTripperProbeReapsForwarderDescendants(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("process-group fixture is POSIX-only")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is unavailable")
	}
	dir := t.TempDir()
	launcher := filepath.Join(dir, "python")
	script := `#!/bin/sh
port=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--port" ]; then port="$2"; shift; fi
  shift
done
"` + python + `" -c '
import http.server, sys
class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.end_headers()
    def log_message(self, *args):
        pass
http.server.HTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
' "$port" &
wait
`
	if err := os.WriteFile(launcher, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPEN_SURFACE_PYTHON", launcher)
	rt := NewBrowserRoundTripper(BrowserRoundTripperConfig{Target: "mysite"})

	begin := time.Now()
	if err := rt.Probe(); err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	if elapsed := time.Since(begin); elapsed > 3*time.Second {
		t.Fatalf("probe left a descendant holding its pipes for %s", elapsed)
	}
}

// ready marks a round tripper as initialized without spawning a subprocess.
func (t *BrowserRoundTripper) ready(port int) {
	t.once.Do(func() {}) // mark start() as done (no-op)
	t.port = port
}

// TestBuildTransportRetryWrapsBrowser guards the layering in BuildTransport:
// when both browser transport and retry are enabled, the retry layer must wrap
// the browser round tripper (not the raw net/http base), otherwise requests
// bypass the browser and get blocked by site risk control.
func TestBuildTransportRetryWrapsBrowser(t *testing.T) {
	opts := Options{Browser: true, BrowserTarget: "mysite", Retry: 3, NoCache: true}
	tr := BuildTransport(opts)
	rt, ok := tr.(retryTransport)
	if !ok {
		t.Fatalf("expected retryTransport layer, got %T", tr)
	}
	if _, ok := rt.inner.(*BrowserRoundTripper); !ok {
		t.Fatalf("retry inner must be *BrowserRoundTripper, got %T", rt.inner)
	}
}

// TestBuildTransportNoRetryUsesBrowser ensures that without retry the browser
// round tripper is the transport handed out directly.
func TestBuildTransportNoRetryUsesBrowser(t *testing.T) {
	opts := Options{Browser: true, BrowserTarget: "mysite", NoCache: true}
	tr := BuildTransport(opts)
	if _, ok := tr.(*BrowserRoundTripper); !ok {
		t.Fatalf("expected *BrowserRoundTripper, got %T", tr)
	}
}

func TestBrowserRoundTripperProtocol(t *testing.T) {
	var got struct {
		Method  string            `json:"method"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
		Target  string            `json:"target"`
	}
	bodyB64 := base64.StdEncoding.EncodeToString([]byte(`{"q":1}`))

	fwd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/request" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode payload: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"status": 200,
			"status_text": "OK",
			"headers": {"content-type": "application/json", "x-flag": "a"},
			"body": "` + base64.StdEncoding.EncodeToString([]byte(`{"ok":true}`)) + `"
		}`))
	}))
	defer fwd.Close()

	port := strings.TrimPrefix(fwd.URL, "http://127.0.0.1:")
	rt := NewBrowserRoundTripper(BrowserRoundTripperConfig{Target: "mysite"})
	rt.ready(atoiSafe(port))

	req, err := http.NewRequest(http.MethodPost, "https://api.example.com/v1/things", strings.NewReader(`{"q":1}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Custom", "abc")

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got.Method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.Method)
	}
	if got.URL != "https://api.example.com/v1/things" {
		t.Errorf("url = %q", got.URL)
	}
	if got.Target != "mysite" {
		t.Errorf("target = %q", got.Target)
	}
	if got.Headers["X-Custom"] != "abc" {
		t.Errorf("headers = %#v", got.Headers)
	}
	if got.Body != bodyB64 {
		t.Errorf("body = %q, want %q", got.Body, bodyB64)
	}

	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}
	if resp.Header.Get("x-flag") != "a" {
		t.Errorf("response header x-flag missing")
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != `{"ok":true}` {
		t.Errorf("response body = %q", string(body))
	}
	if resp.ContentLength != int64(len(body)) {
		t.Errorf("content length = %d, want %d", resp.ContentLength, len(body))
	}
}

func TestBrowserRoundTripperPassesThroughStatus(t *testing.T) {
	fwd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status": 406, "status_text": "Not Acceptable", "headers": {}, "body": ""}`))
	}))
	defer fwd.Close()

	port := strings.TrimPrefix(fwd.URL, "http://127.0.0.1:")
	rt := NewBrowserRoundTripper(BrowserRoundTripperConfig{Target: "x"})
	rt.ready(atoiSafe(port))

	req, _ := http.NewRequest(http.MethodGet, "https://api.example.com/v1/me", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 406 {
		t.Errorf("status = %d, want 406 (status codes must pass through)", resp.StatusCode)
	}
}

// TestBrowserRoundTripperStripsBodyEncodingHeaders guards the gzip fix: the
// forwarder returns an already-decoded body (the browser's fetch decompressed
// it), so a Content-Encoding/Content-Length describing the original wire form
// must not survive — otherwise the client tries to gunzip plaintext
// ("gzip: invalid header") or truncates to the compressed length.
func TestBrowserRoundTripperStripsBodyEncodingHeaders(t *testing.T) {
	plain := `{"ok":true,"note":"decoded"}`
	fwd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"status": 200,
			"headers": {
				"Content-Type": "application/json",
				"Content-Encoding": "gzip",
				"Content-Length": "17",
				"Transfer-Encoding": "chunked",
				"X-Keep": "y"
			},
			"body": "` + base64.StdEncoding.EncodeToString([]byte(plain)) + `"
		}`))
	}))
	defer fwd.Close()

	port := strings.TrimPrefix(fwd.URL, "http://127.0.0.1:")
	rt := NewBrowserRoundTripper(BrowserRoundTripperConfig{Target: "mysite"})
	rt.ready(atoiSafe(port))

	req, _ := http.NewRequest(http.MethodGet, "https://api.example.com/x", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding must be stripped, got %q", got)
	}
	if got := resp.Header.Get("Transfer-Encoding"); got != "" {
		t.Errorf("Transfer-Encoding must be stripped, got %q", got)
	}
	if resp.Header.Get("X-Keep") != "y" {
		t.Errorf("unrelated header X-Keep must survive")
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != plain {
		t.Errorf("body = %q, want %q", string(body), plain)
	}
	// Content-Length must reflect the decoded body, not the stale "17".
	if resp.ContentLength != int64(len(plain)) {
		t.Errorf("content length = %d, want %d", resp.ContentLength, len(plain))
	}
	if resp.Header.Get("Content-Length") != "" && resp.Header.Get("Content-Length") == "17" {
		t.Errorf("stale Content-Length 17 must not survive")
	}
}

func atoiSafe(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			continue
		}
		n = n*10 + int(c-'0')
	}
	return n
}
