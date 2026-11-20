package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"graphallthethings/internal/engine"
	"graphallthethings/internal/mcpserver"
)

func mcpText(t string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: t}}}
}

type globalEmptyIn struct{}

type globalRepoOnlyIn struct {
	Repo string `json:"repo,omitempty" jsonschema:"absolute path of the current repository (e.g. /mnt/code/myrepo) or unique repo name; omit if only one repo is registered"`
}

type globalContextIn struct {
	Repo      string `json:"repo,omitempty" jsonschema:"absolute path of the target repository (or unique repo name)"`
	Question  string `json:"question" jsonschema:"natural-language question about the code"`
	Limit     int    `json:"limit,omitempty" jsonschema:"max items to include, default 6"`
	MaxTokens int    `json:"max_tokens,omitempty" jsonschema:"optional approximate token budget (e.g. 1500 for compact, 4000 for deep); 0 = default"`
}

type globalTestsIn struct {
	Repo   string `json:"repo,omitempty" jsonschema:"absolute path of the target repository (or unique repo name)"`
	Target string `json:"target,omitempty" jsonschema:"file path, symbol name, node id, or 'diff:HEAD' (default: current git diff vs HEAD)"`
	Depth  int    `json:"depth,omitempty" jsonschema:"dependency hops to walk, default 3"`
}

type globalImpactIn struct {
	Repo      string `json:"repo,omitempty" jsonschema:"absolute path of the target repository (or unique repo name)"`
	Function  string `json:"function" jsonschema:"function name or full node id"`
	Depth     int    `json:"depth,omitempty" jsonschema:"caller levels to walk, default 3"`
	MaxTokens int    `json:"max_tokens,omitempty" jsonschema:"optional approximate token budget; 0 = unlimited"`
}

type globalBlastIn struct {
	Repo      string `json:"repo,omitempty" jsonschema:"absolute path of the target repository (or unique repo name)"`
	Target    string `json:"target" jsonschema:"file path, function name, or full node id"`
	Depth     int    `json:"depth,omitempty" jsonschema:"dependency levels to walk, default 3"`
	MaxTokens int    `json:"max_tokens,omitempty" jsonschema:"optional approximate token budget; 0 = unlimited"`
}

type globalTracePathIn struct {
	Repo     string `json:"repo,omitempty" jsonschema:"absolute path of the target repository (or unique repo name)"`
	From     string `json:"from" jsonschema:"source node: route (e.g. 'POST /checkout'), function name, file path, model name, or full node id"`
	To       string `json:"to" jsonschema:"target node: function name, model/table name, file path, route, or full node id"`
	MaxDepth int    `json:"max_depth,omitempty" jsonschema:"max hops to walk, default 8 (max 15)"`
}

type globalReadSymbolsIn struct {
	Repo      string   `json:"repo,omitempty" jsonschema:"absolute path of the target repository (or unique repo name)"`
	IDs       []string `json:"ids" jsonschema:"list of function/type/file names or full node ids to read in one batch"`
	MaxTokens int      `json:"max_tokens,omitempty" jsonschema:"approximate token budget, default 4000"`
}

type globalPreflightIn struct {
	Repo      string `json:"repo,omitempty" jsonschema:"absolute path of the target repository (or unique repo name)"`
	Ref       string `json:"ref,omitempty" jsonschema:"git ref to check against, default HEAD"`
	MaxTokens int    `json:"max_tokens,omitempty" jsonschema:"optional approximate token budget; 0 = unlimited"`
}

type globalHotspotsIn struct {
	Repo  string `json:"repo,omitempty" jsonschema:"absolute path of the target repository (or unique repo name)"`
	Limit int    `json:"limit,omitempty" jsonschema:"max hotspot files to return, default 20"`
}

type globalFileHistoryIn struct {
	Repo  string `json:"repo,omitempty" jsonschema:"absolute path of the target repository (or unique repo name)"`
	File  string `json:"file" jsonschema:"repository-relative file path"`
	Limit int    `json:"limit,omitempty" jsonschema:"max recent commits to return, default 8"`
}

type globalAnnotateIn struct {
	Repo           string `json:"repo,omitempty" jsonschema:"absolute path of the target repository (or unique repo name)"`
	Node           string `json:"node" jsonschema:"function/type/file/table name or node id to annotate"`
	EntityNote     string `json:"entity_note,omitempty" jsonschema:"free-text architectural or business note"`
	DefaultFilter  string `json:"default_filter,omitempty" jsonschema:"canonical WHERE clause for DB entities"`
	Invariant      string `json:"invariant,omitempty" jsonschema:"code invariant surfaced in impact/blast/read_symbols/preflight_check"`
	Gotcha         string `json:"gotcha,omitempty" jsonschema:"non-obvious pitfall to remember before editing"`
	RouteMethod    string `json:"route_method,omitempty" jsonschema:"HTTP method for manual route tagging"`
	RoutePath      string `json:"route_path,omitempty" jsonschema:"route path for manual route tagging"`
	RouteFramework string `json:"route_framework,omitempty" jsonschema:"web framework name for manual route tagging"`
	Clear          bool   `json:"clear,omitempty" jsonschema:"remove all annotations from the node"`
}

type globalGrepIn struct {
	Repo    string   `json:"repo,omitempty" jsonschema:"absolute path of the target repository (or unique repo name)"`
	Pattern string   `json:"pattern" jsonschema:"literal text (default) or regex (with regex=true)"`
	Regex   bool     `json:"regex,omitempty" jsonschema:"treat pattern as case-insensitive regex"`
	Limit   *int     `json:"limit,omitempty" jsonschema:"max matches to display; default 50, 0 = unlimited"`
	Include []string `json:"include,omitempty" jsonschema:"optional glob filters to include (e.g. '*.ts', 'src/**')"`
	Exclude []string `json:"exclude,omitempty" jsonschema:"optional glob filters to exclude (e.g. '*_test.go', 'docs/**')"`
	All     bool     `json:"all,omitempty" jsonschema:"include lockfiles skipped by default"`
}

type globalTreeIn struct {
	Repo  string `json:"repo,omitempty" jsonschema:"absolute path of the target repository (or unique repo name)"`
	Path  string `json:"path,omitempty" jsonschema:"relative path to scope the tree to"`
	Depth int    `json:"depth,omitempty" jsonschema:"max path segments deep, 0 = unlimited"`
}

type globalFileFilterIn struct {
	Repo      string `json:"repo,omitempty" jsonschema:"absolute path of the target repository (or unique repo name)"`
	File      string `json:"file,omitempty" jsonschema:"only entries in files whose path contains this substring"`
	MaxTokens int    `json:"max_tokens,omitempty" jsonschema:"optional approximate token budget; 0 = unlimited"`
}

type globalCodeDiffIn struct {
	Repo      string `json:"repo,omitempty" jsonschema:"absolute path of the target repository (or unique repo name)"`
	Ref       string `json:"ref,omitempty" jsonschema:"git ref to diff against, default HEAD"`
	Limit     int    `json:"limit,omitempty" jsonschema:"max changes to display, default 30"`
	MaxTokens int    `json:"max_tokens,omitempty" jsonschema:"optional approximate token budget; 0 = unlimited"`
}

type globalDocDriftIn struct {
	Repo  string `json:"repo,omitempty" jsonschema:"absolute path of the target repository (or unique repo name)"`
	Limit int    `json:"limit,omitempty" jsonschema:"max docs to report, default 15"`
}

type globalFindIn struct {
	Repo  string `json:"repo,omitempty" jsonschema:"absolute path of the target repository (or unique repo name)"`
	Query string `json:"query" jsonschema:"natural-language description or symbol name to find"`
	Type  string `json:"type,omitempty" jsonschema:"optional node type filter: function, definition, file, comment"`
	Limit int    `json:"limit,omitempty" jsonschema:"max results, default 8"`
}

type globalDescribeIn struct {
	Repo string `json:"repo,omitempty" jsonschema:"absolute path of the target repository (or unique repo name)"`
	ID   string `json:"id" jsonschema:"symbol name or full node id"`
}

// newGlobalMCPHandler serves a single global MCP Streamable HTTP endpoint at
// `/mcp` (with no fixed `?repo=` query parameter) for user-scoped installations
// (`gatt install --http http://127.0.0.1:7788 --scope user`). Every tool takes
// a `repo` parameter (absolute path or unique basename) and routes to repoPool.
func newGlobalMCPHandler(pool *repoPool) http.Handler {
	srv := mcp.NewServer(&mcp.Implementation{Name: "graphallthethings", Version: "0.3.2"}, nil)
	mcpserver.RegisterPrompts(srv, true)

	resolveEngine := func(ctx context.Context, repo string, autoRefresh bool) (*engine.Engine, string, error) {
		entry, err := pool.Resolve(ctx, repo)
		if err != nil {
			return nil, "", err
		}
		return entry.mcp.Engine(ctx, autoRefresh)
	}

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_repos",
		Description: "List all repositories indexed by the central gatt server (their mounted paths, current git HEAD, and background indexing status). Call this if you are unsure what `repo` value to pass to other tools.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ globalEmptyIn) (*mcp.CallToolResult, any, error) {
		repos := pool.List()
		if len(repos) == 0 {
			return mcpText("no repositories loaded yet; pass an absolute mounted path as `repo` to any tool to extract it on demand"), nil, nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%d repository(ies) registered:\n", len(repos))
		for _, r := range repos {
			state := "ready"
			if r.Running {
				if r.Progress != nil && r.Progress.Total > 0 {
					state = fmt.Sprintf("indexing %d/%d", r.Progress.Done, r.Progress.Total)
				} else {
					state = "indexing"
				}
			}
			head := ""
			if r.Head != "" {
				head = " @" + r.Head
			}
			fmt.Fprintf(&b, "- %s%s [%s]\n", r.Root, head, state)
		}
		return mcpText(b.String()), nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "code_context",
		Description: "PREFERRED FIRST CALL for any code question on a codebase graph. Pass `repo` set to the current repository's absolute path (or unique folder name). Returns relevant functions (signatures, file:line, callers/callees), types/consts, and docs in one compact block.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in globalContextIn) (*mcp.CallToolResult, any, error) {
		e, note, err := resolveEngine(ctx, in.Repo, true)
		if err != nil {
			return nil, nil, err
		}
		out, err := e.CodeContextPackWithBudget(ctx, in.Question, in.Limit, in.MaxTokens)
		if err != nil {
			return nil, nil, err
		}
		return mcpText(note + out), nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "affected_tests",
		Description: "Minimal set of test files, direct test functions, and ready-to-run test commands impacted by a file, symbol, or current git diff. Pass `repo`.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in globalTestsIn) (*mcp.CallToolResult, any, error) {
		e, note, err := resolveEngine(ctx, in.Repo, true)
		if err != nil {
			return nil, nil, err
		}
		res, err := e.AffectedTests(ctx, in.Target, in.Depth)
		if err != nil {
			return nil, nil, err
		}
		return mcpText(note + res.Text), nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "impact",
		Description: "Transitive callers of a function up to N levels — call BEFORE changing any function signature or behavior. Pass `repo` set to the repository's absolute path (or unique name).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in globalImpactIn) (*mcp.CallToolResult, any, error) {
		e, note, err := resolveEngine(ctx, in.Repo, true)
		if err != nil {
			return nil, nil, err
		}
		out, err := e.Impact(in.Function, in.Depth)
		if err != nil {
			return nil, nil, err
		}
		return mcpText(note + engine.TrimToTokenBudget(out, in.MaxTokens)), nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "trace_path",
		Description: "Shortest execution/data-flow path between any two codebase nodes (route, function, event emitter/listener, sqlc query, ORM model, or file). Pass `repo`.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in globalTracePathIn) (*mcp.CallToolResult, any, error) {
		e, note, err := resolveEngine(ctx, in.Repo, true)
		if err != nil {
			return nil, nil, err
		}
		out, err := e.TracePath(in.From, in.To, in.MaxDepth)
		if err != nil {
			return nil, nil, err
		}
		return mcpText(note + out), nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "read_symbols",
		Description: "Batch-read the full source body (with line numbers) and annotations (entity_note, invariant, gotcha) of 1..N symbols in one call. Pass `repo`.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in globalReadSymbolsIn) (*mcp.CallToolResult, any, error) {
		e, note, err := resolveEngine(ctx, in.Repo, true)
		if err != nil {
			return nil, nil, err
		}
		out, err := e.ReadSymbols(in.IDs, in.MaxTokens)
		if err != nil {
			return nil, nil, err
		}
		return mcpText(note + out), nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "preflight_check",
		Description: "Composite post-edit verification gate vs git diff (default HEAD): risk alerts (generated files, hotspots, gotchas/invariants), structural diff, affected tests + commands, and affected docs. Pass `repo`.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in globalPreflightIn) (*mcp.CallToolResult, any, error) {
		e, note, err := resolveEngine(ctx, in.Repo, true)
		if err != nil {
			return nil, nil, err
		}
		out, err := e.PreflightCheck(ctx, in.Ref, in.MaxTokens)
		if err != nil {
			return nil, nil, err
		}
		return mcpText(note + out), nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "hotspots",
		Description: "Rank repository files by recent git churn × graph connectivity, surfacing high-risk architectural hubs. Pass `repo`.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in globalHotspotsIn) (*mcp.CallToolResult, any, error) {
		e, note, err := resolveEngine(ctx, in.Repo, true)
		if err != nil {
			return nil, nil, err
		}
		limit := in.Limit
		if limit <= 0 {
			limit = 20
		}
		entries, err := e.Hotspots(ctx, limit)
		if err != nil {
			return nil, nil, err
		}
		return mcpText(note + engine.RenderHotspots(entries)), nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "file_history",
		Description: "Recent git commit history for a file, including extracted issue references (#42, PROJ-123). Pass `repo`.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in globalFileHistoryIn) (*mcp.CallToolResult, any, error) {
		e, note, err := resolveEngine(ctx, in.Repo, true)
		if err != nil {
			return nil, nil, err
		}
		entries := e.FileHistory(ctx, in.File, in.Limit)
		return mcpText(note + engine.RenderFileHistory(in.File, entries)), nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "annotate_entity",
		Description: "Persist architectural/business notes (entity_note, invariant, gotcha, default_filter, route_*) onto a node so future sessions carry it. Pass `repo`.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in globalAnnotateIn) (*mcp.CallToolResult, any, error) {
		entry, err := pool.Resolve(ctx, in.Repo)
		if err != nil {
			return nil, nil, err
		}
		return entry.mcp.Annotate(mcpserver.AnnotateIn{
			Node:           in.Node,
			EntityNote:     in.EntityNote,
			DefaultFilter:  in.DefaultFilter,
			Invariant:      in.Invariant,
			Gotcha:         in.Gotcha,
			RouteMethod:    in.RouteMethod,
			RoutePath:      in.RoutePath,
			RouteFramework: in.RouteFramework,
			Clear:          in.Clear,
		})
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "blast",
		Description: "Blast radius of modifying ANY node (file, function, or type): transitive callers, importers, generated copies, git co-change companions, and shared mutable state. Pass `repo`.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in globalBlastIn) (*mcp.CallToolResult, any, error) {
		e, note, err := resolveEngine(ctx, in.Repo, true)
		if err != nil {
			return nil, nil, err
		}
		out, err := e.Blast(in.Target, in.Depth)
		if err != nil {
			return nil, nil, err
		}
		return mcpText(note + engine.TrimToTokenBudget(out, in.MaxTokens)), nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "grep",
		Description: "Exhaustive literal or regex search across every file in the repository (zero hits = proof of absence). Pass `limit: 0` for unlimited results during audits, and optional `include`/`exclude` globs. Pass `repo`.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in globalGrepIn) (*mcp.CallToolResult, any, error) {
		e, note, err := resolveEngine(ctx, in.Repo, true)
		if err != nil {
			return nil, nil, err
		}
		limit := 50
		if in.Limit != nil {
			limit = *in.Limit
		}
		res, err := e.GrepWithOptions(in.Pattern, engine.GrepOptions{
			Regex:            in.Regex,
			Limit:            limit,
			Include:          in.Include,
			Exclude:          in.Exclude,
			IncludeLockfiles: in.All,
		})
		if err != nil {
			return nil, nil, err
		}
		return mcpText(note + res.Text), nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "tree",
		Description: "Directory tree of the repository annotated with each file's doc summary. Pass `repo`.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in globalTreeIn) (*mcp.CallToolResult, any, error) {
		e, note, err := resolveEngine(ctx, in.Repo, true)
		if err != nil {
			return nil, nil, err
		}
		out, err := e.Tree(in.Path, in.Depth)
		if err != nil {
			return nil, nil, err
		}
		return mcpText(note + out), nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "routes",
		Description: "HTTP routes detected in the repository (method, path, handler, middleware, touched ORM models, and client call sites). Pass `repo`.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in globalFileFilterIn) (*mcp.CallToolResult, any, error) {
		e, note, err := resolveEngine(ctx, in.Repo, true)
		if err != nil {
			return nil, nil, err
		}
		out, err := e.Routes(in.File)
		if err != nil {
			return nil, nil, err
		}
		return mcpText(note + engine.TrimToTokenBudget(out, in.MaxTokens)), nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "models",
		Description: "ORM models detected in the repository (name, table, fields, associations). Pass `repo`.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in globalFileFilterIn) (*mcp.CallToolResult, any, error) {
		e, note, err := resolveEngine(ctx, in.Repo, true)
		if err != nil {
			return nil, nil, err
		}
		out, err := e.Models(in.File)
		if err != nil {
			return nil, nil, err
		}
		return mcpText(note + out), nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "code_diff",
		Description: "Structural diff between the working tree and a git ref (default HEAD): added/removed/changed/renamed/moved functions and types plus callers. Pass `repo`.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in globalCodeDiffIn) (*mcp.CallToolResult, any, error) {
		e, note, err := resolveEngine(ctx, in.Repo, true)
		if err != nil {
			return nil, nil, err
		}
		out, err := e.CodeDiff(ctx, in.Ref, in.Limit)
		if err != nil {
			return nil, nil, err
		}
		return mcpText(note + engine.TrimToTokenBudget(out, in.MaxTokens)), nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "doc_drift",
		Description: "Markdown docs whose code references no longer resolve or lag behind recent commits. Pass `repo`.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in globalDocDriftIn) (*mcp.CallToolResult, any, error) {
		e, note, err := resolveEngine(ctx, in.Repo, true)
		if err != nil {
			return nil, nil, err
		}
		out, err := e.DocDrift(in.Limit)
		if err != nil {
			return nil, nil, err
		}
		return mcpText(note + out), nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "find_entities",
		Description: "Semantic + keyword search for functions, types, files, or comments in a repository. Pass `repo`.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in globalFindIn) (*mcp.CallToolResult, any, error) {
		e, note, err := resolveEngine(ctx, in.Repo, true)
		if err != nil {
			return nil, nil, err
		}
		limit := in.Limit
		if limit <= 0 {
			limit = 8
		}
		res, err := e.Find(ctx, in.Query, in.Type, limit)
		if err != nil {
			return nil, nil, err
		}
		return mcpText(note + e.RenderFind(res)), nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "describe_entity",
		Description: "Full details of a single node (attributes, signature, doc, edges). Pass `repo`.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in globalDescribeIn) (*mcp.CallToolResult, any, error) {
		e, note, err := resolveEngine(ctx, in.Repo, true)
		if err != nil {
			return nil, nil, err
		}
		out, err := e.Render(in.ID)
		if err != nil {
			return nil, nil, err
		}
		return mcpText(note + out), nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "graph_overview",
		Description: "High-level node counts and freshness of a repository's graph. Pass `repo`.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in globalRepoOnlyIn) (*mcp.CallToolResult, any, error) {
		e, note, err := resolveEngine(ctx, in.Repo, false)
		if err != nil {
			return nil, nil, err
		}
		ov := e.Overview()
		var b strings.Builder
		fmt.Fprintf(&b, "source: %s\n", ov.Source)
		for k, v := range ov.NodeCounts {
			fmt.Fprintf(&b, "- %s: %d\n", k, v)
		}
		return mcpText(note + b.String()), nil, nil
	})

	srv.AddResource(&mcp.Resource{
		URI:         "gatt://repos",
		Name:        "repos",
		Title:       "Registered Repositories",
		Description: "All repositories indexed by the central gatt server (paths, git HEADs, indexing status).",
		MIMEType:    "text/plain",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		repos := pool.List()
		var b strings.Builder
		if len(repos) == 0 {
			b.WriteString("no repositories loaded yet\n")
		} else {
			fmt.Fprintf(&b, "%d repository(ies) registered:\n", len(repos))
			for _, r := range repos {
				state := "ready"
				if r.Running {
					state = "indexing"
				}
				head := ""
				if r.Head != "" {
					head = " @" + r.Head
				}
				fmt.Fprintf(&b, "- %s%s [%s]\n", r.Root, head, state)
			}
		}
		return &mcp.ReadResourceResult{
			Contents: []*mcp.ResourceContents{
				{URI: req.Params.URI, MIMEType: "text/plain", Text: b.String()},
			},
		}, nil
	})

	addGlobalRepoTemplate := func(name, title, view, desc string, render func(ctx context.Context, e *engine.Engine) (string, error)) {
		srv.AddResourceTemplate(&mcp.ResourceTemplate{
			URITemplate: "gatt://repo/{repo}/" + view,
			Name:        name,
			Title:       title,
			Description: desc,
			MIMEType:    "text/plain",
		}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			prefix := "gatt://repo/"
			suffix := "/" + view
			uri := req.Params.URI
			if !strings.HasPrefix(uri, prefix) || !strings.HasSuffix(uri, suffix) {
				return nil, mcp.ResourceNotFoundError(uri)
			}
			repoName := strings.TrimSuffix(strings.TrimPrefix(uri, prefix), suffix)
			e, note, err := resolveEngine(ctx, repoName, view != "overview")
			if err != nil {
				return nil, err
			}
			body, err := render(ctx, e)
			if err != nil {
				return nil, err
			}
			return &mcp.ReadResourceResult{
				Contents: []*mcp.ResourceContents{
					{URI: uri, MIMEType: "text/plain", Text: note + body},
				},
			}, nil
		})
	}
	addGlobalRepoTemplate("repo-overview", "Repository Overview", "overview", "Graph overview for a specific repository.", func(ctx context.Context, e *engine.Engine) (string, error) {
		return e.RenderOverview(), nil
	})
	addGlobalRepoTemplate("repo-hotspots", "Repository Hotspots", "hotspots", "Top 20 hotspots for a specific repository.", func(ctx context.Context, e *engine.Engine) (string, error) {
		entries, err := e.Hotspots(ctx, 20)
		if err != nil {
			return "", err
		}
		return engine.RenderHotspots(entries), nil
	})
	addGlobalRepoTemplate("repo-annotations", "Repository Annotations", "annotations", "Persisted agent annotations (invariants, gotchas, notes) for a specific repository.", func(ctx context.Context, e *engine.Engine) (string, error) {
		return mcpserver.RenderAnnotations(e), nil
	})
	addGlobalRepoTemplate("repo-routes", "Repository Routes", "routes", "Detected HTTP routes for a specific repository.", func(ctx context.Context, e *engine.Engine) (string, error) {
		return e.Routes("")
	})
	addGlobalRepoTemplate("repo-models", "Repository Models", "models", "Detected ORM and sqlc models for a specific repository.", func(ctx context.Context, e *engine.Engine) (string, error) {
		return e.Models("")
	})

	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return srv
	}, nil)
}
