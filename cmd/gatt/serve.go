package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"graphallthethings/internal/embed"
	"graphallthethings/internal/graph"
	"graphallthethings/internal/indexer"
	"graphallthethings/internal/store"
	"graphallthethings/internal/store/local"
)

const defaultServeAddr = "127.0.0.1:7788"

// serveAddr is the default address for serve/notify; GATT_ADDR overrides it.
func serveAddr() string {
	if a := os.Getenv("GATT_ADDR"); a != "" {
		return a
	}
	return defaultServeAddr
}

// syncStatus is what one background pass did; served at GET /status.
type syncStatus struct {
	At       time.Time `json:"at"`
	Duration string    `json:"duration"`
	Summary  string    `json:"summary,omitempty"` // graph refresh summary; empty = no drift
	Embedded int       `json:"embedded"`          // nodes sent to the embedder
	Note     string    `json:"note,omitempty"`    // non-fatal remark (e.g. embeddings skipped)
	Error    string    `json:"error,omitempty"`   // fatal for this pass
}

// coalescer runs work in the background, never concurrently with itself.
// Triggers that arrive while a pass is running collapse into exactly one more
// pass afterwards, so a burst of hooks costs two passes, not N.
type coalescer struct {
	mu      sync.Mutex
	running bool
	dirty   bool
	last    *syncStatus
	work    func() syncStatus
	done    chan struct{} // closed-and-replaced after each pass (tests wait on it)
}

func newCoalescer(work func() syncStatus) *coalescer {
	return &coalescer{work: work, done: make(chan struct{})}
}

// Trigger requests a pass. It returns "started" when it launched one, or
// "queued" when a pass is already running and another will follow it.
func (c *coalescer) Trigger() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running {
		c.dirty = true
		return "queued"
	}
	c.running = true
	go c.loop()
	return "started"
}

func (c *coalescer) loop() {
	for {
		st := c.work()
		c.mu.Lock()
		c.last = &st
		old := c.done
		c.done = make(chan struct{})
		again := c.dirty
		c.dirty = false
		if !again {
			c.running = false
		}
		c.mu.Unlock()
		close(old)
		if !again {
			return
		}
	}
}

// Status returns whether a pass is running or queued, and the last result.
func (c *coalescer) Status() (running, queued bool, last *syncStatus) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.running, c.dirty, c.last
}

// loopbackHost reports whether host (no port) is a loopback name or address.
func loopbackHost(host string) bool {
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// guard rejects anything that is not a plain local, non-browser request:
// a Host that is not loopback (DNS rebinding) or any Origin header (a web page
// POSTing to localhost). The hook client (curl, `gatt notify`) sends neither.
func guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if !loopbackHost(host) || r.Header.Get("Origin") != "" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func newServeHandler(c *coalescer) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/refresh", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]string{"state": c.Trigger()})
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		running, queued, last := c.Status()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"running": running, "queued": queued, "last": last})
	})
	return guard(mux)
}

// cmdServe keeps a graph fresh in the background. The agent's hook calls
// `gatt notify` (or POSTs /refresh); each pass re-parses drifted files, then
// brings the vectors up to date with the same hash-incremental logic as
// `gatt index`. Queries never wait on any of it.
func cmdServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	graphPath, qdURL, coll, embURL, embModel := indexFlags(fs)
	addr := fs.String("addr", serveAddr(), "listen address (loopback only)")
	noEmbed := fs.Bool("no-embed", false, "refresh the graph only; never call the embedding server")
	onnx := fs.String("onnx", os.Getenv("GATT_EMBED_WORKER"), "path to the gatt-embed worker (tools/gatt-embed or its server.mjs): serve starts it, embeds with a local ONNX model and stops it on exit")
	onnxPort := fs.Int("onnx-port", 7801, "port for the onnx worker (fixed so queries can find it via GATT_EMBED_URL)")
	onnxModel := fs.String("onnx-model", "", "model for the onnx worker (default: the worker's multilingual MiniLM; downloaded on first use)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	host, _, err := net.SplitHostPort(*addr)
	if err != nil {
		return fmt.Errorf("--addr: %w", err)
	}
	if !loopbackHost(host) {
		return fmt.Errorf("--addr %q: serve only listens on loopback (it is unauthenticated)", *addr)
	}

	passCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	sc := syncConfig{graphPath: *graphPath, qdURL: *qdURL, coll: *coll, embURL: *embURL, embModel: *embModel, noEmbed: *noEmbed}
	if *onnx != "" {
		w, err := startOnnxWorker(passCtx, *onnx, *onnxPort, *onnxModel)
		if err != nil {
			return err
		}
		defer w.Stop()
		fmt.Fprintf(os.Stderr, "gatt serve: onnx worker ready at %s (model %s)\n", w.URL, w.Model)
		fmt.Fprintf(os.Stderr, "gatt serve: for queries to use the same model, export GATT_EMBED_URL=%s\n", w.URL)
		sc.embURL, sc.initIndex = w.URL, true
		if sc.embModel == "" {
			sc.embModel = w.Model
		}
	}
	c := newCoalescer(func() syncStatus { return syncOnce(passCtx, sc) })
	srv := &http.Server{Addr: *addr, Handler: newServeHandler(c), ReadHeaderTimeout: 5 * time.Second}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		cancel()
		shutdown, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		srv.Shutdown(shutdown)
	}()

	fmt.Fprintf(os.Stderr, "gatt serve: listening on http://%s (POST /refresh, GET /status)\n", *addr)
	c.Trigger() // bring everything current once at startup
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// syncConfig is everything one background pass needs.
type syncConfig struct {
	graphPath, qdURL, coll, embURL, embModel string
	noEmbed                                  bool
	// initIndex builds the vector index from scratch when none exists. Only
	// safe with a local embedder (--onnx): with a remote or slow one, the first
	// full embedding would be a surprise, so it stays an explicit `gatt index`.
	initIndex bool
}

// syncOnce is one background pass: refresh the graph, then catch the vectors up.
func syncOnce(ctx context.Context, c syncConfig) (st syncStatus) {
	graphPath, qdURL, coll, embURL, embModel, noEmbed := c.graphPath, c.qdURL, c.coll, c.embURL, c.embModel, c.noEmbed
	start := time.Now()
	st.At = start
	defer func() { st.Duration = time.Since(start).Round(time.Millisecond).String() }()

	// Qdrant has no per-node hash cache, so keep its inline (added-nodes) path.
	inline := qdURL != "" && !noEmbed
	res, err := refreshCodebase(ctx, graphPath, qdURL, coll, embURL, embModel, inline)
	if err != nil {
		st.Error = err.Error()
		return st
	}
	st.Summary, st.Embedded = res.Summary, res.Embedded
	if noEmbed || inline {
		return st
	}
	if !vectorsExist(graphPath, qdURL) && !c.initIndex {
		st.Note = "no vector index yet; run `gatt index` once to enable semantic search"
		return st
	}

	g, err := graph.Load(graphPath)
	if err != nil {
		st.Error = err.Error()
		return st
	}
	vs := openStore(graphPath, qdURL, coll)
	model := resolveModel(embModel, vs)
	if stored := localStoredModel(vs); stored != "" && stored != model {
		st.Note = fmt.Sprintf("vectors were built with %q, not %q; skipped (run `gatt index --full` to switch)", stored, model)
		return st
	}
	r, err := indexer.Reindex(ctx, g, vs, embed.New(embURL, model), model, false, nil)
	if err != nil {
		st.Note = "embedding skipped: " + err.Error() // an unreachable embedder is not fatal
		return st
	}
	st.Embedded += r.Embedded
	return st
}

// localStoredModel returns the model recorded in a local vector index, or ""
// for any other backend or an empty index.
func localStoredModel(vs store.VectorStore) string {
	if ls, ok := vs.(*local.Store); ok {
		return ls.StoredModel()
	}
	return ""
}

// cmdNotify tells a running `gatt serve` to refresh. It is meant for agent
// hooks, so it is fast and never fails the hook: if nothing is listening it
// exits 0 quietly (use -v to see why).
func cmdNotify(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("notify", flag.ExitOnError)
	addr := fs.String("addr", serveAddr(), "address of gatt serve")
	verbose := fs.Bool("v", false, "report failures on stderr")
	if err := fs.Parse(args); err != nil {
		return err
	}
	hc := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := hc.Post("http://"+*addr+"/refresh", "application/json", nil)
	if err != nil {
		if *verbose {
			fmt.Fprintln(os.Stderr, "gatt notify: no server:", err)
		}
		return nil
	}
	resp.Body.Close()
	if *verbose && resp.StatusCode != http.StatusAccepted {
		fmt.Fprintln(os.Stderr, "gatt notify: unexpected status", resp.Status)
	}
	return nil
}

// onnxWorker is a running gatt-embed child process.
type onnxWorker struct {
	URL   string // OpenAI-compatible base, e.g. http://127.0.0.1:7801/v1
	Model string // model name the worker reports
	cmd   *exec.Cmd
}

var workerListening = regexp.MustCompile(`listening on (http://127\.0\.0\.1:\d+) model=(\S+)`)

// startOnnxWorker launches `node <script> --port N` and waits until it prints
// its listen line. path may be the worker directory or its server.mjs. The
// model itself is downloaded lazily by the worker on the first embedding.
func startOnnxWorker(ctx context.Context, path string, port int, model string) (*onnxWorker, error) {
	script := path
	if fi, err := os.Stat(path); err == nil && fi.IsDir() {
		script = filepath.Join(path, "server.mjs")
	}
	if _, err := os.Stat(script); err != nil {
		return nil, fmt.Errorf("--onnx: %w", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(script), "node_modules")); err != nil {
		return nil, fmt.Errorf("--onnx: dependencies missing; run `npm install` in %s", filepath.Dir(script))
	}
	node, err := exec.LookPath("node")
	if err != nil {
		return nil, fmt.Errorf("--onnx needs node (>=18) on PATH: %w", err)
	}
	args := []string{script, "--port", strconv.Itoa(port)}
	if model != "" {
		args = append(args, "--model", model)
	}
	cmd := exec.CommandContext(ctx, node, args...)
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	w := &onnxWorker{cmd: cmd}

	type hit struct{ url, model string }
	ready := make(chan hit, 1)
	go func() {
		sc := bufio.NewScanner(out)
		sent := false
		for sc.Scan() { // keep draining so the child never blocks on a full pipe
			if m := workerListening.FindStringSubmatch(sc.Text()); m != nil && !sent {
				ready <- hit{m[1], m[2]}
				sent = true
			}
		}
	}()
	select {
	case h := <-ready:
		w.URL, w.Model = h.url+"/v1", h.model
		return w, nil
	case <-time.After(20 * time.Second):
		w.Stop()
		return nil, errors.New("--onnx: worker did not start within 20s")
	case <-ctx.Done():
		w.Stop()
		return nil, ctx.Err()
	}
}

// Stop asks the worker to exit, then kills it if it does not.
func (w *onnxWorker) Stop() {
	if w == nil || w.cmd == nil || w.cmd.Process == nil {
		return
	}
	_ = w.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _ = w.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = w.cmd.Process.Kill()
		<-done
	}
}

func envOr(key, dflt string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return dflt
}
