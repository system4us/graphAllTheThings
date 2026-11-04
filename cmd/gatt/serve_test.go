package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// A burst of triggers during a running pass must collapse into one extra pass,
// and passes must never overlap.
func TestCoalescerCollapsesBurst(t *testing.T) {
	var passes, concurrent, maxConc atomic.Int32
	gate := make(chan struct{})
	c := newCoalescer(func() syncStatus {
		n := concurrent.Add(1)
		if n > maxConc.Load() {
			maxConc.Store(n)
		}
		passes.Add(1)
		<-gate
		concurrent.Add(-1)
		return syncStatus{}
	})

	if got := c.Trigger(); got != "started" {
		t.Fatalf("first trigger = %q, want started", got)
	}
	waitFor(t, func() bool { return passes.Load() == 1 })
	for i := 0; i < 20; i++ {
		if got := c.Trigger(); got != "queued" {
			t.Fatalf("trigger during run = %q, want queued", got)
		}
	}
	gate <- struct{}{} // finish pass 1 -> exactly one follow-up pass
	waitFor(t, func() bool { return passes.Load() == 2 })
	gate <- struct{}{}
	waitFor(t, func() bool { r, _, _ := c.Status(); return !r })

	if passes.Load() != 2 {
		t.Errorf("passes = %d, want 2 (1 + one coalesced follow-up)", passes.Load())
	}
	if maxConc.Load() != 1 {
		t.Errorf("passes overlapped (max concurrency %d)", maxConc.Load())
	}
	if _, _, last := c.Status(); last == nil {
		t.Error("no last status recorded")
	}
	// Idle again: a new trigger starts a fresh pass.
	var once sync.Once
	go once.Do(func() { gate <- struct{}{} })
	if got := c.Trigger(); got != "started" {
		t.Errorf("trigger when idle = %q, want started", got)
	}
}

func TestGuardRejectsRebindingAndBrowsers(t *testing.T) {
	c := newCoalescer(func() syncStatus { return syncStatus{} })
	h := newServeHandler(c)
	do := func(method, host, origin string) int {
		r := httptest.NewRequest(method, "http://"+host+"/status", nil)
		r.Host = host
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	cases := []struct {
		name, method, host, origin string
		want                       int
	}{
		{"loopback ok", "GET", "127.0.0.1:7788", "", http.StatusOK},
		{"localhost ok", "GET", "localhost:7788", "", http.StatusOK},
		{"ipv6 loopback ok", "GET", "[::1]:7788", "", http.StatusOK},
		{"dns rebinding", "GET", "evil.example.com:7788", "", http.StatusForbidden},
		{"browser origin", "GET", "127.0.0.1:7788", "https://evil.example.com", http.StatusForbidden},
	}
	for _, tc := range cases {
		if got := do(tc.method, tc.host, tc.origin); got != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestRefreshEndpointIsPostOnly(t *testing.T) {
	var passes atomic.Int32
	c := newCoalescer(func() syncStatus { passes.Add(1); return syncStatus{} })
	srv := httptest.NewServer(newServeHandler(c))
	defer srv.Close()

	resp, _ := http.Get(srv.URL + "/refresh")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /refresh = %d, want 405", resp.StatusCode)
	}
	resp, err := http.Post(srv.URL+"/refresh", "application/json", nil)
	if err != nil || resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /refresh = %v, %v", resp, err)
	}
	waitFor(t, func() bool { return passes.Load() == 1 })
}

func TestLoopbackHost(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1": true, "localhost": true, "::1": true, "[::1]": true,
		"0.0.0.0": false, "192.168.1.5": false, "example.com": false, "": false,
	} {
		if got := loopbackHost(host); got != want {
			t.Errorf("loopbackHost(%q) = %v, want %v", host, got, want)
		}
	}
	if !strings.Contains(defaultServeAddr, "127.0.0.1") {
		t.Error("default serve address must be loopback")
	}
}

func TestNotifyIsSilentWithoutServer(t *testing.T) {
	// Nothing listens on port 1; a hook must still exit cleanly.
	if err := cmdNotify(nil, []string{"-addr", "127.0.0.1:1"}); err != nil {
		t.Errorf("notify with no server returned %v, want nil", err)
	}
}

func TestSyncOnceReportsDurationAndMissingIndex(t *testing.T) {
	path := t.TempDir() + "/graph.db" // no graph, no vectors.json
	st := syncOnce(context.Background(), syncConfig{graphPath: path, coll: "gatt"})
	if st.Duration == "" || st.At.IsZero() {
		t.Errorf("duration/at not set: %+v", st)
	}
	if !strings.Contains(st.Note, "gatt index") || st.Error != "" {
		t.Errorf("want a note pointing at `gatt index` and no error, got %+v", st)
	}
	if st := syncOnce(context.Background(), syncConfig{graphPath: path, coll: "gatt", noEmbed: true}); st.Note != "" || st.Duration == "" {
		t.Errorf("--no-embed must not touch embeddings: %+v", st)
	}
}

// A stand-in worker: prints the listen line, then waits for SIGTERM.
const fakeWorker = `
console.log('gatt-embed listening on http://127.0.0.1:' + process.argv[3] + ' model=fake/model');
process.on('SIGTERM', () => process.exit(0));
setInterval(() => {}, 1000);
`

func TestOnnxWorkerLifecycle(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "server.mjs"), []byte(fakeWorker), 0o644); err != nil {
		t.Fatal(err)
	}
	// Dependencies check: without node_modules the error must say what to do.
	if _, err := startOnnxWorker(context.Background(), dir, 7999, ""); err == nil || !strings.Contains(err.Error(), "npm install") {
		t.Fatalf("missing node_modules: err = %v, want an npm install hint", err)
	}
	os.Mkdir(filepath.Join(dir, "node_modules"), 0o755)

	w, err := startOnnxWorker(context.Background(), dir, 7999, "")
	if err != nil {
		t.Fatal(err)
	}
	if w.URL != "http://127.0.0.1:7999/v1" || w.Model != "fake/model" {
		t.Errorf("got URL=%q Model=%q", w.URL, w.Model)
	}
	pid := w.cmd.Process.Pid
	w.Stop()
	if err := syscall.Kill(pid, 0); err == nil {
		t.Error("worker still running after Stop")
	}
}

func TestOnnxWorkerBadPath(t *testing.T) {
	if _, err := startOnnxWorker(context.Background(), "/nonexistent/dir", 7999, ""); err == nil {
		t.Error("expected an error for a missing worker path")
	}
}

func TestStatusReportsProgressWhileRunning(t *testing.T) {
	gate := make(chan struct{})
	var c *coalescer
	c = newCoalescer(func() syncStatus {
		c.SetProgress(30, 100)
		<-gate
		return syncStatus{}
	})
	srv := httptest.NewServer(newServeHandler(c))
	defer srv.Close()
	c.Trigger()
	waitFor(t, func() bool { return c.pTotal.Load() == 100 })

	resp, err := http.Get(srv.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Running  bool                        `json:"running"`
		Progress struct{ Done, Total int64 } `json:"progress"`
	}
	json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if !got.Running || got.Progress.Done != 30 || got.Progress.Total != 100 {
		t.Errorf("status = %+v, want running with 30/100", got)
	}
	close(gate)
	waitFor(t, func() bool { r, _, _ := c.Status(); return !r })
	if c.pTotal.Load() != 0 {
		t.Error("progress not reset after the pass")
	}
}

func TestVectorsFresh(t *testing.T) {
	dir := t.TempDir()
	graphPath := filepath.Join(dir, "graph.db")
	if vectorsFresh(graphPath) {
		t.Error("no files: must not be fresh")
	}
	os.WriteFile(graphPath, []byte("g1"), 0o644)
	os.WriteFile(vectorsPath(graphPath), []byte("{}"), 0o644)
	if vectorsFresh(graphPath) {
		t.Error("vectors present but never caught up (no stamp): must not be fresh")
	}
	markVectorsFresh(graphPath, graphSig(graphPath))
	if !vectorsFresh(graphPath) {
		t.Error("after catching up the vectors must be fresh")
	}

	// A query-time refresh rewrites the graph: no longer fresh.
	os.WriteFile(graphPath, []byte("g2-longer"), 0o644)
	if vectorsFresh(graphPath) {
		t.Error("graph changed after the catch-up: must not be fresh")
	}

	// Restoring an OLDER graph with the same size must not look fresh either
	// (an mtime comparison with vectors.json would have said it was).
	markVectorsFresh(graphPath, graphSig(graphPath))
	old := time.Now().Add(-48 * time.Hour)
	os.Chtimes(graphPath, old, old)
	if vectorsFresh(graphPath) {
		t.Error("graph restored from an older copy: must not be fresh")
	}

	// The graph changing WHILE we embed must leave it stale: stamp the signature
	// taken before reading, not the one after.
	sigBefore := graphSig(graphPath)
	os.WriteFile(graphPath, []byte("changed during embedding"), 0o644)
	markVectorsFresh(graphPath, sigBefore)
	if vectorsFresh(graphPath) {
		t.Error("graph changed during the catch-up: stamp must not match")
	}
}
