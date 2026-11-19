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
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"graphallthethings/internal/embed"
	"graphallthethings/internal/graph"
	"graphallthethings/internal/httpapi"
	"graphallthethings/internal/indexer"
	"graphallthethings/internal/mcpserver"
	"graphallthethings/internal/store"
	"graphallthethings/internal/store/local"
	"graphallthethings/internal/webui"
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

	// Progress of the embedding step of the running pass (0/0 when idle).
	pDone, pTotal atomic.Int64
}

// SetProgress records embedding progress for GET /status.
func (c *coalescer) SetProgress(done, total int) {
	c.pDone.Store(int64(done))
	c.pTotal.Store(int64(total))
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
		c.SetProgress(0, 0)
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

// sameOriginAllowed reports whether an Origin header matches the request's Host
// and that host is permitted (allowing the embedded web UI to POST to its own
// server while blocking cross-origin pages such as https://evil.example.com).
func sameOriginAllowed(origin, reqHost, extraHost string) bool {
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	if !strings.EqualFold(u.Host, reqHost) {
		return false
	}
	return allowedHost(u.Hostname(), extraHost)
}

func allowedHost(host, extraHosts string) bool {
	if loopbackHost(host) {
		return true
	}
	clean := strings.Trim(host, "[]")
	for _, raw := range strings.Split(extraHosts, ",") {
		eh := strings.Trim(strings.TrimSpace(raw), "[]")
		if eh != "" && eh != "0.0.0.0" && eh != "::" && strings.EqualFold(clean, eh) {
			return true
		}
	}
	return false
}

// guard rejects anything that is not a plain local request: a Host that is not
// loopback (DNS rebinding) or a cross-origin browser request.
func guard(next http.Handler) http.Handler {
	return guardWithHost(next, "")
}

type guardedHandler struct {
	inner     http.Handler
	extraHost string
}

func (g *guardedHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}
	if !allowedHost(host, g.extraHost) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" && !sameOriginAllowed(origin, r.Host, g.extraHost) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	g.inner.ServeHTTP(w, r)
}

func unwrapGuard(h http.Handler) http.Handler {
	if g, ok := h.(*guardedHandler); ok {
		return g.inner
	}
	return h
}

// guardWithHost is like guard, additionally permitting requests whose Host
// matches extraHost (e.g. an explicitly bound Tailscale IP with --bind-any).
func guardWithHost(next http.Handler, extraHost string) http.Handler {
	return &guardedHandler{inner: next, extraHost: extraHost}
}

func newServeHandler(c *coalescer) http.Handler {
	return newSingleRepoHandler(c, nil, nil)
}

func newSingleRepoHandler(c *coalescer, mcpHandler http.Handler, apiProvider httpapi.Provider) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
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
		out := map[string]any{"running": running, "queued": queued, "last": last}
		if running && c.pTotal.Load() > 0 {
			out["progress"] = map[string]int64{"done": c.pDone.Load(), "total": c.pTotal.Load()}
		}
		json.NewEncoder(w).Encode(out)
	})
	if mcpHandler != nil {
		mux.Handle("/mcp", mcpHandler)
	}
	if apiProvider != nil {
		mux.Handle("/api/", httpapi.New(apiProvider))
	}
	mux.Handle("/", webui.Handler())
	return guard(mux)
}

type onnxStatus struct {
	URL   string `json:"url,omitempty"`
	Model string `json:"model,omitempty"`
}

func writeRepoError(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, errBadRepo):
		code = http.StatusBadRequest
	case errors.Is(err, errRepoNotMounted):
		code = http.StatusNotFound
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func newMultiRepoHandler(pool *repoPool, onnxInfo *onnxStatus, scanDirs ...string) http.Handler {
	globalMCP := newGlobalMCPHandler(pool)
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		out := map[string]any{"ok": true, "mode": "multi-repo"}
		if onnxInfo != nil {
			out["model"] = onnxInfo.Model
		}
		if pool.baseSync.qdURL != "" {
			out["vector_store"] = "qdrant"
		} else {
			out["vector_store"] = "local"
		}
		json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("/repos", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"repos": pool.List()})
	})
	mux.HandleFunc("/refresh", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		if repo := requestRepo(r); repo != "" {
			entry, err := pool.Get(r.Context(), repo)
			if err != nil {
				writeRepoError(w, err)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(map[string]string{"repo": entry.root, "state": entry.coalescer.Trigger()})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]any{"repos": pool.RefreshAll()})
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		if repo := requestRepo(r); repo != "" {
			entry, err := pool.Get(r.Context(), repo)
			if err != nil {
				writeRepoError(w, err)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(entry.status())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		out := map[string]any{"repos": pool.List()}
		if onnxInfo != nil {
			out["onnx"] = onnxInfo
		}
		if pool.baseSync.qdURL != "" {
			out["qdrant"] = map[string]any{"url": pool.baseSync.qdURL}
		}
		if len(scanDirs) > 0 {
			out["scan_dirs"] = scanDirs
		}
		json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		repo := requestRepo(r)
		if repo == "" {
			// Global user-scoped MCP endpoint: tools take a `repo` parameter.
			globalMCP.ServeHTTP(w, r)
			return
		}
		entry, err := pool.Get(r.Context(), repo)
		if err != nil {
			writeRepoError(w, err)
			return
		}
		entry.mcpHandler.ServeHTTP(w, r)
	})
	mux.Handle("/api/", httpapi.New(&multiRepoProvider{pool: pool}))
	mux.Handle("/", webui.Handler())
	return guard(mux)
}

// cmdServe keeps a graph fresh in the background and optionally serves MCP over
// Streamable HTTP. With --data-dir it runs in multi-repo mode, serving any
// mounted repository selected via ?repo=/abs/path (or X-Gatt-Repo).
func cmdServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	graphPath, qdURL, coll, embURL, embModel := indexFlags(fs)
	addr := fs.String("addr", serveAddr(), "listen address (loopback only unless --bind-any)")
	bindAny := fs.Bool("bind-any", os.Getenv("GATT_BIND_ANY") == "1", "allow binding a non-loopback address (e.g. 0.0.0.0 inside Docker; Host header is still restricted to loopback)")
	enableMCP := fs.Bool("mcp", false, "expose MCP over Streamable HTTP at /mcp (always on with --data-dir)")
	dataDir := fs.String("data-dir", os.Getenv("GATT_DATA_DIR"), "multi-repo mode: directory where per-repo graphs and vectors are stored")
	reposFlag := fs.String("repos", os.Getenv("GATT_REPOS"), "comma-separated absolute repo paths to preload on startup (multi-repo mode)")
	scanDirsFlag := fs.String("scan-dirs", os.Getenv("GATT_SCAN_DIRS"), "comma-separated mounted parent directories to auto-discover git repos in (multi-repo mode)")
	defaultScanDepth := 2
	if v := os.Getenv("GATT_SCAN_DEPTH"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			defaultScanDepth = n
		}
	}
	scanDepth := fs.Int("scan-depth", defaultScanDepth, "max directory depth under --scan-dirs to look for .git repos")
	noEmbed := fs.Bool("no-embed", false, "refresh the graph only; never call the embedding server")
	onnx := fs.String("onnx", os.Getenv("GATT_EMBED_WORKER"), "path to the gatt-embed worker (tools/gatt-embed or its server.mjs): serve starts it, embeds with a local ONNX model and stops it on exit")
	onnxPort := fs.Int("onnx-port", 7801, "port for the onnx worker (fixed so queries can find it via GATT_EMBED_URL)")
	onnxModel := fs.String("onnx-model", "", "model for the onnx worker (default: the worker's multilingual MiniLM; downloaded on first use)")
	qdrantBin := fs.String("qdrant-bin", os.Getenv("GATT_QDRANT_BIN"), "path to an embedded qdrant binary: serve starts it on loopback (127.0.0.1) with storage under --data-dir/qdrant and stops it on exit")
	qdrantPort := fs.Int("qdrant-port", 6333, "HTTP port for the embedded qdrant process (loopback only)")
	defaultWatch := 5 * time.Second
	if v := os.Getenv("GATT_WATCH_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			defaultWatch = d
		}
	}
	watchInterval := fs.Duration("watch-interval", defaultWatch, "poll interval to detect git commits and trigger incremental reindex (0 disables)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	host, _, err := net.SplitHostPort(*addr)
	if err != nil {
		return fmt.Errorf("--addr: %w", err)
	}
	if !*bindAny && !loopbackHost(host) {
		return fmt.Errorf("--addr %q: serve only listens on loopback (pass --bind-any inside a container published on 127.0.0.1)", *addr)
	}

	passCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	sc := syncConfig{graphPath: *graphPath, qdURL: *qdURL, coll: *coll, embURL: *embURL, embModel: *embModel, noEmbed: *noEmbed}
	if *qdrantBin != "" && sc.qdURL == "" && !*noEmbed {
		storageRoot := filepath.Join(filepath.Dir(*graphPath), "qdrant")
		if *dataDir != "" {
			storageRoot = filepath.Join(*dataDir, "qdrant")
		}
		qp, err := startQdrant(passCtx, *qdrantBin, storageRoot, *qdrantPort)
		if err != nil {
			return err
		}
		defer qp.Stop()
		sc.qdURL = qp.URL
		fmt.Fprintf(os.Stderr, "gatt serve: embedded qdrant ready at %s (storage %s)\n", qp.URL, storageRoot)
	}
	var onnxInfo *onnxStatus
	if *onnx != "" {
		w, err := startOnnxWorker(passCtx, *onnx, *onnxPort, *onnxModel)
		if err != nil {
			return err
		}
		defer w.Stop()
		onnxInfo = &onnxStatus{URL: w.URL, Model: w.Model}
		fmt.Fprintf(os.Stderr, "gatt serve: onnx worker ready at %s (model %s)\n", w.URL, w.Model)
		if *dataDir == "" && !*enableMCP {
			fmt.Fprintf(os.Stderr, "gatt serve: for queries to use the same model, export GATT_EMBED_URL=%s\n", w.URL)
		}
		sc.embURL, sc.initIndex = w.URL, true
		if sc.embModel == "" {
			sc.embModel = w.Model
		}
	}

	var handler http.Handler
	if *dataDir != "" {
		if err := os.MkdirAll(*dataDir, 0o755); err != nil {
			return fmt.Errorf("--data-dir: %w", err)
		}
		pool := newRepoPool(passCtx, *dataDir, sc)
		var extra []string
		if *reposFlag != "" {
			extra = strings.Split(*reposFlag, ",")
		}
		var scanDirs []string
		if *scanDirsFlag != "" {
			for _, d := range strings.Split(*scanDirsFlag, ",") {
				if s := containerRepoPath(d); s != "" {
					scanDirs = append(scanDirs, s)
				}
			}
		}
		go pool.LoadExisting(passCtx, extra, scanDirs, *scanDepth)
		if len(scanDirs) > 0 {
			go func() {
				t := time.NewTicker(60 * time.Second)
				defer t.Stop()
				for {
					select {
					case <-passCtx.Done():
						return
					case <-t.C:
						pool.ScanNew(passCtx, scanDirs, *scanDepth)
					}
				}
			}()
		}
		go watchGitCommits(passCtx, *watchInterval, pool.Roots, func(root, _ string) {
			pool.Trigger(root)
		})
		handler = newMultiRepoHandler(pool, onnxInfo, scanDirs...)
		fmt.Fprintf(os.Stderr, "gatt serve: multi-repo MCP HTTP listening on http://%s (/mcp [global or ?repo=<path>], POST /refresh, GET /status, GET /repos, GET /health)\n", *addr)
	} else {
		var c *coalescer
		var mcpSrv *mcpserver.Server
		var mcpHandler http.Handler
		if *enableMCP {
			mcpCfg := mcpserver.Config{
				GraphPath: *graphPath,
				OnAutoRefresh: func() {
					if c != nil {
						c.Trigger()
					}
				},
			}
			if !sc.noEmbed {
				vs := openStore(*graphPath, sc.qdURL, sc.coll)
				model := sc.embModel
				if model == "" {
					model = resolveModel("", vs)
				}
				mcpCfg.OpenStore = func() store.VectorStore { return openStore(*graphPath, sc.qdURL, sc.coll) }
				mcpCfg.Embedder = embed.New(sc.embURL, model)
				mcpCfg.EmbModel = model
			}
			var err error
			mcpSrv, err = mcpserver.New(mcpCfg)
			if err != nil {
				return fmt.Errorf("mcp init: %w", err)
			}
			mcpHandler = mcpSrv.HTTPHandler(nil)
		}
		c = newCoalescer(func() syncStatus {
			st := syncOnce(passCtx, sc)
			if mcpSrv != nil && st.Error == "" && (st.Summary != "" || st.Embedded > 0) {
				_ = mcpSrv.Reload()
			}
			return st
		})
		sc.progress = c.SetProgress
		apiProv := &singleRepoProvider{
			graphPath: *graphPath,
			coalescer: c,
			mcpSrv:    mcpSrv,
		}
		handler = newSingleRepoHandler(c, mcpHandler, apiProv)
		if *enableMCP {
			fmt.Fprintf(os.Stderr, "gatt serve: listening on http://%s (Web UI /, /api/*, /mcp, POST /refresh, GET /status, GET /health)\n", *addr)
		} else {
			fmt.Fprintf(os.Stderr, "gatt serve: listening on http://%s (Web UI /, /api/*, POST /refresh, GET /status, GET /health)\n", *addr)
		}
		c.Trigger() // bring everything current once at startup
		go watchGitCommits(passCtx, *watchInterval, func() []string {
			if src, _, err := graph.LoadCodebaseState(*graphPath); err == nil && strings.HasPrefix(src, "codebase:") {
				return []string{strings.TrimPrefix(src, "codebase:")}
			}
			return nil
		}, func(_, _ string) {
			c.Trigger()
		})
	}

	var extraHosts []string
	if *bindAny && !loopbackHost(host) && host != "0.0.0.0" && host != "::" && host != "" {
		extraHosts = append(extraHosts, host)
	}
	if envHosts := strings.TrimSpace(os.Getenv("GATT_ALLOWED_HOSTS")); envHosts != "" {
		extraHosts = append(extraHosts, envHosts)
	}
	if len(extraHosts) > 0 {
		handler = guardWithHost(unwrapGuard(handler), strings.Join(extraHosts, ","))
	}
	srv := &http.Server{Addr: *addr, Handler: handler, ReadHeaderTimeout: 5 * time.Second}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		cancel()
		shutdown, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		srv.Shutdown(shutdown)
	}()

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
	// progress, if set, is called after each embedded batch.
	progress func(done, total int)
}

// syncOnce is one background pass: refresh the graph, then catch the vectors up.
func syncOnce(ctx context.Context, c syncConfig) (st syncStatus) {
	graphPath, qdURL, coll, embURL, embModel, noEmbed := c.graphPath, c.qdURL, c.coll, c.embURL, c.embModel, c.noEmbed
	start := time.Now()
	st.At = start
	defer func() { st.Duration = time.Since(start).Round(time.Millisecond).String() }()

	res, err := refreshCodebase(ctx, graphPath, qdURL, coll, embURL, embModel, false)
	if err != nil {
		st.Error = err.Error()
		return st
	}
	st.Summary, st.Embedded = res.Summary, res.Embedded
	if noEmbed {
		return st
	}
	if !vectorsExist(graphPath, qdURL) && !c.initIndex {
		st.Note = "no vector index yet; run `gatt index` once to enable semantic search"
		return st
	}

	// No drift this pass and the vectors are newer than the graph: nothing can be
	// pending, so skip loading and hashing the whole index (seconds on big repos).
	// A query-time refresh rewrites graph.db without embedding, which makes the
	// vectors older and lets the next pass catch up.
	if res.Summary == "" && qdURL == "" && vectorsFresh(graphPath) {
		return st
	}
	sig := graphSig(graphPath)
	g, err := graph.Load(graphPath)
	if err != nil {
		st.Error = err.Error()
		return st
	}
	vs := openStore(graphPath, qdURL, coll)
	model := resolveIndexModel(ctx, embModel, embURL, vs, false)
	if stored := localStoredModel(vs); stored != "" && stored != model {
		st.Note = fmt.Sprintf("vectors were built with %q, not %q; skipped (run `gatt index --full` to switch)", stored, model)
		return st
	}
	r, err := indexer.Reindex(ctx, g, vs, embed.New(embURL, model), model, false, c.progress)
	if err != nil {
		st.Note = "embedding skipped: " + err.Error() // an unreachable embedder is not fatal
		return st
	}
	st.Embedded += r.Embedded
	if qdURL == "" {
		markVectorsFresh(graphPath, sig) // also when nothing needed embedding
	}
	return st
}

func vectorsPath(graphPath string) string {
	return filepath.Join(filepath.Dir(graphPath), "vectors.json")
}

// graphSig identifies the exact graph file a catch-up ran against (size and
// mtime in ns). Unlike comparing mtimes with vectors.json it is immune to clock
// changes and to restoring an older copy of either file.
func graphSig(graphPath string) string {
	st, err := os.Stat(graphPath)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d:%d", st.Size(), st.ModTime().UnixNano())
}

func stampPath(graphPath string) string {
	return filepath.Join(filepath.Dir(graphPath), ".vectors-stamp")
}

// vectorsFresh reports whether the vectors were caught up against this very graph.
func vectorsFresh(graphPath string) bool {
	if _, err := os.Stat(vectorsPath(graphPath)); err != nil {
		return false
	}
	want := graphSig(graphPath)
	got, err := os.ReadFile(stampPath(graphPath))
	return want != "" && err == nil && string(got) == want
}

// markVectorsFresh records that the vectors are caught up with the graph whose
// signature was sig. Take sig BEFORE reading the graph: if it changes while we
// embed, the stamp then no longer matches and the next pass catches up again.
func markVectorsFresh(graphPath, sig string) {
	if sig != "" {
		_ = os.WriteFile(stampPath(graphPath), []byte(sig), 0o644)
	}
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
// exits 0 quietly (use -v to see why). In multi-repo mode, --repo (default:
// current working directory) selects which mounted repository to refresh.
func cmdNotify(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("notify", flag.ExitOnError)
	addr := fs.String("addr", serveAddr(), "address of gatt serve")
	defaultRepo, _ := os.Getwd()
	repo := fs.String("repo", defaultRepo, "repository path to refresh in multi-repo mode (empty = all)")
	verbose := fs.Bool("v", false, "report failures on stderr")
	if err := fs.Parse(args); err != nil {
		return err
	}
	endpoint := "http://" + *addr + "/refresh"
	if r := strings.TrimSpace(*repo); r != "" {
		if abs, err := filepath.Abs(r); err == nil {
			r = abs
		}
		endpoint += "?repo=" + url.QueryEscape(containerRepoPath(r))
	}
	hc := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := hc.Post(endpoint, "application/json", nil)
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

// qdrantProc is a running embedded Qdrant child process bound strictly to
// loopback inside the container/host.
type qdrantProc struct {
	URL string
	cmd *exec.Cmd
}

func startQdrant(ctx context.Context, binPath, storageRoot string, port int) (*qdrantProc, error) {
	if _, err := os.Stat(binPath); err != nil {
		return nil, fmt.Errorf("--qdrant-bin: %w", err)
	}
	storageDir := filepath.Join(storageRoot, "storage")
	snapshotsDir := filepath.Join(storageRoot, "snapshots")
	if err := os.MkdirAll(storageDir, 0o755); err != nil {
		return nil, fmt.Errorf("--qdrant-bin: create storage dir: %w", err)
	}
	if err := os.MkdirAll(snapshotsDir, 0o755); err != nil {
		return nil, fmt.Errorf("--qdrant-bin: create snapshots dir: %w", err)
	}

	cmd := exec.CommandContext(ctx, binPath)
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(),
		"QDRANT__SERVICE__HOST=127.0.0.1",
		"QDRANT__SERVICE__HTTP_PORT="+strconv.Itoa(port),
		"QDRANT__SERVICE__GRPC_PORT="+strconv.Itoa(port+1),
		"QDRANT__SERVICE__ENABLE_STATIC_CONTENT=false",
		"QDRANT__TELEMETRY_DISABLED=true",
		"QDRANT__STORAGE__STORAGE_PATH="+storageDir,
		"QDRANT__STORAGE__SNAPSHOTS_PATH="+snapshotsDir,
		"QDRANT__LOG_LEVEL=WARN",
	)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("--qdrant-bin: start: %w", err)
	}

	qp := &qdrantProc{URL: fmt.Sprintf("http://127.0.0.1:%d", port), cmd: cmd}
	hc := &http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			qp.Stop()
			return nil, ctx.Err()
		}
		resp, err := hc.Get(qp.URL + "/readyz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return qp, nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	qp.Stop()
	return nil, errors.New("--qdrant-bin: qdrant did not become ready within 20s")
}

// Stop sends SIGTERM to Qdrant so it flushes storage cleanly, then kills if needed.
func (q *qdrantProc) Stop() {
	if q == nil || q.cmd == nil || q.cmd.Process == nil {
		return
	}
	_ = q.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _ = q.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = q.cmd.Process.Kill()
		<-done
	}
}

func envOr(key, dflt string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return dflt
}
