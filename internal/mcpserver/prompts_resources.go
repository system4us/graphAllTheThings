package mcpserver

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/system4us/graphAllTheThings/internal/engine"
)

// RegisterPrompts registers guided agentic workflow prompts on any MCP server
// (both single-repo stdio/HTTP and the global multi-repo HTTP server). When
// includeRepoArg is true, each prompt also declares an optional `repo` argument.
func RegisterPrompts(srv *mcp.Server, includeRepoArg bool) {
	maybeRepoArg := func(args []*mcp.PromptArgument) []*mcp.PromptArgument {
		if !includeRepoArg {
			return args
		}
		return append(args, &mcp.PromptArgument{
			Name:        "repo",
			Description: "Target repository absolute path or unique folder name (required on the global /mcp endpoint).",
			Required:    false,
		})
	}
	repoHint := func(args map[string]string) string {
		if !includeRepoArg {
			return ""
		}
		if r := strings.TrimSpace(args["repo"]); r != "" {
			return fmt.Sprintf("Pass `repo: %q` to every gatt tool call.\n\n", r)
		}
		return "Pass `repo` (absolute path or unique folder name; call `list_repos` if unsure) to every gatt tool call.\n\n"
	}

	srv.AddPrompt(&mcp.Prompt{
		Name:        "plan-change",
		Title:       "Plan a code change using the graph",
		Description: "Graph-first planning workflow: locate relevant symbols, inspect full bodies, check impact/blast/hotspots/history, and produce a file:line plan before editing.",
		Arguments: maybeRepoArg([]*mcp.PromptArgument{
			{Name: "task", Description: "What feature, refactor, or fix you want to plan.", Required: true},
		}),
	}, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		task := strings.TrimSpace(req.Params.Arguments["task"])
		if task == "" {
			task = "(describe the change)"
		}
		body := fmt.Sprintf(`%sPlan the following change using gatt's graph tools before reading whole files or editing:

Task: %s

Steps:
1. Call code_context(question=%q, max_tokens=2500) to map the relevant functions, types, routes, models, and docs.
2. Call read_symbols(ids=[...]) in one batch to read the exact bodies and any persisted invariant/gotcha notes of the symbols you expect to touch.
3. For every function signature/behavior you plan to change, call impact(function=...). For shared config, schema, or sqlc .sql files, call blast(target=...).
4. Call file_history(file=...) on any unfamiliar or hotspot file you plan to modify so you don't regress recent fixes.
5. Output a concise, concrete implementation plan quoting exact file:line locations, risks/invariants, and the tests to run afterward.`, repoHint(req.Params.Arguments), task, task)
		return &mcp.GetPromptResult{
			Description: "Graph-first planning workflow",
			Messages: []*mcp.PromptMessage{
				{Role: "user", Content: &mcp.TextContent{Text: body}},
			},
		}, nil
	})

	srv.AddPrompt(&mcp.Prompt{
		Name:        "debug-flow",
		Title:       "Trace and debug an execution or data flow",
		Description: "End-to-end debugging workflow across HTTP routes, function calls, string-keyed event dispatch, sqlc queries, and ORM models.",
		Arguments: maybeRepoArg([]*mcp.PromptArgument{
			{Name: "symptom", Description: "Bug symptom, failing behavior, or flow to investigate.", Required: true},
			{Name: "from", Description: "Optional starting node (e.g. route 'POST /orders' or entry function).", Required: false},
			{Name: "to", Description: "Optional target node (e.g. model/table name or sink function).", Required: false},
		}),
	}, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		symptom := strings.TrimSpace(req.Params.Arguments["symptom"])
		from := strings.TrimSpace(req.Params.Arguments["from"])
		to := strings.TrimSpace(req.Params.Arguments["to"])
		traceStep := "2. Once you identify the entry point and sink, call trace_path(from=..., to=...) to trace the path across routes, calls, event dispatch, sqlc queries, and models."
		if from != "" && to != "" {
			traceStep = fmt.Sprintf("2. Call trace_path(from=%q, to=%q) to inspect the exact execution/data-flow path across routes, calls, event dispatch, sqlc queries, and models.", from, to)
		}
		body := fmt.Sprintf(`%sDebug the following issue using gatt's graph tools:

Symptom: %s

Steps:
1. Call code_context(question=%q, max_tokens=2500) to locate the entry point, handlers, and data layer involved.
%s
3. Call read_symbols(ids=[...]) on the key hops along the path to inspect the implementation and any invariant/gotcha annotations.
4. Call file_history(file=...) on the suspect files to see if a recent commit introduced the regression.
5. Summarize the root cause with exact file:line evidence and propose a minimal fix.`, repoHint(req.Params.Arguments), symptom, symptom, traceStep)
		return &mcp.GetPromptResult{
			Description: "Graph-first flow debugging workflow",
			Messages: []*mcp.PromptMessage{
				{Role: "user", Content: &mcp.TextContent{Text: body}},
			},
		}, nil
	})

	srv.AddPrompt(&mcp.Prompt{
		Name:        "review-diff",
		Title:       "Review current changes before committing",
		Description: "Post-edit verification workflow combining preflight_check, risk alerts, affected tests, and doc drift.",
		Arguments: maybeRepoArg([]*mcp.PromptArgument{
			{Name: "ref", Description: "Git ref to diff against (default: HEAD).", Required: false},
		}),
	}, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		ref := strings.TrimSpace(req.Params.Arguments["ref"])
		if ref == "" {
			ref = "HEAD"
		}
		body := fmt.Sprintf(`%sReview the working-tree changes against %s using gatt:

Steps:
1. Call preflight_check(ref=%q) to get risk alerts (edited generated files, hotspots, gotchas/invariants), the structural diff with affected callers, affected tests, and docs mentioning changed symbols.
2. If any high-risk function or generated file was touched, call read_symbols(ids=[...]) or blast(target=...) to verify callers and invariants.
3. Run the recommended test commands from affected_tests.
4. Report any real bugs, missing updates to callers/docs, or invariant violations — or confirm the diff is ready to commit.`, repoHint(req.Params.Arguments), ref, ref)
		return &mcp.GetPromptResult{
			Description: "Post-edit preflight review workflow",
			Messages: []*mcp.PromptMessage{
				{Role: "user", Content: &mcp.TextContent{Text: body}},
			},
		}, nil
	})

	srv.AddPrompt(&mcp.Prompt{
		Name:        "onboard-repo",
		Title:       "Onboard onto an unfamiliar codebase",
		Description: "Low-token orientation workflow: overview, annotated tree, hotspots, routes, and models.",
		Arguments: maybeRepoArg([]*mcp.PromptArgument{
			{Name: "focus", Description: "Optional subsystem or question to focus on.", Required: false},
		}),
	}, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		focus := strings.TrimSpace(req.Params.Arguments["focus"])
		focusStep := ""
		if focus != "" {
			focusStep = fmt.Sprintf("\n5. Call code_context(question=%q, max_tokens=2500) to deep-dive into the requested focus area.", focus)
		}
		body := fmt.Sprintf(`%sOrient on this repository using gatt in minimal tokens:

Steps:
1. Call graph_overview() and tree(depth=2) to understand the project layout and main packages.
2. Call hotspots(limit=15) to identify the most active, central files.
3. Call routes(max_tokens=2000) and models() to map the HTTP surface and data layer (ORM / sqlc).
4. Read the gatt://annotations resource (if available) to inherit persisted architectural invariants and gotchas.%s
Summarize the architecture, key entry points, data layer, and high-risk areas.`, repoHint(req.Params.Arguments), focusStep)
		return &mcp.GetPromptResult{
			Description: "Low-token repository onboarding workflow",
			Messages: []*mcp.PromptMessage{
				{Role: "user", Content: &mcp.TextContent{Text: body}},
			},
		}, nil
	})
}

// RenderAnnotations returns a compact text list of every node carrying
// persisted agent/business annotations (entity_note, default_filter, invariant,
// gotcha, route_*).
func RenderAnnotations(e *engine.Engine) string {
	annKeys := []string{"entity_note", "default_filter", "invariant", "gotcha", "route_method", "route_path", "route_framework"}
	type entry struct {
		id    string
		name  string
		typ   string
		lines []string
	}
	var entries []entry
	for _, n := range e.G.Nodes {
		var lines []string
		for _, k := range annKeys {
			if v := n.Attrs[k]; v != "" {
				lines = append(lines, fmt.Sprintf("  %s: %s", k, v))
			}
		}
		if len(lines) > 0 {
			entries = append(entries, entry{id: n.ID, name: n.Name, typ: n.Type, lines: lines})
		}
	}
	if len(entries) == 0 {
		return "no annotations recorded yet; use annotate_entity to persist invariants, gotchas, or business notes.\n"
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].id < entries[j].id })
	var b strings.Builder
	fmt.Fprintf(&b, "%d annotated node(s):\n", len(entries))
	for _, en := range entries {
		fmt.Fprintf(&b, "- [%s] %s (%s)\n", en.typ, en.name, en.id)
		for _, l := range en.lines {
			b.WriteString(l)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func (s *Server) registerPromptsAndResources() {
	RegisterPrompts(s.server, false)

	resText := func(uri, body string) *mcp.ReadResourceResult {
		return &mcp.ReadResourceResult{
			Contents: []*mcp.ResourceContents{
				{URI: uri, MIMEType: "text/plain", Text: body},
			},
		}
	}

	s.server.AddResource(&mcp.Resource{
		URI:         "gatt://overview",
		Name:        "overview",
		Title:       "Graph Overview",
		Description: "High-level node counts, freshness, and entity summary of the current graph.",
		MIMEType:    "text/plain",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		e, err := s.requireEngine()
		if err != nil {
			return nil, err
		}
		return resText(req.Params.URI, e.RenderOverview()), nil
	})

	s.server.AddResource(&mcp.Resource{
		URI:         "gatt://hotspots",
		Name:        "hotspots",
		Title:       "Codebase Hotspots",
		Description: "Top 20 codebase files ranked by recent git churn × graph connectivity.",
		MIMEType:    "text/plain",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		note := s.autoRefreshCodebase(ctx)
		e, err := s.requireEngine()
		if err != nil {
			return nil, err
		}
		entries, err := e.Hotspots(ctx, 20)
		if err != nil {
			return nil, err
		}
		return resText(req.Params.URI, note+engine.RenderHotspots(entries)), nil
	})

	s.server.AddResource(&mcp.Resource{
		URI:         "gatt://annotations",
		Name:        "annotations",
		Title:       "Persisted Annotations",
		Description: "All curated entity notes, default filters, code invariants, gotchas, and manual route tags.",
		MIMEType:    "text/plain",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		e, err := s.requireEngine()
		if err != nil {
			return nil, err
		}
		return resText(req.Params.URI, RenderAnnotations(e)), nil
	})

	s.server.AddResource(&mcp.Resource{
		URI:         "gatt://routes",
		Name:        "routes",
		Title:       "HTTP Routes",
		Description: "All detected HTTP routes, handlers, middleware, touched models, and client call sites.",
		MIMEType:    "text/plain",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		note := s.autoRefreshCodebase(ctx)
		e, err := s.requireEngine()
		if err != nil {
			return nil, err
		}
		out, err := e.Routes("")
		if err != nil {
			return nil, err
		}
		return resText(req.Params.URI, note+out), nil
	})

	s.server.AddResource(&mcp.Resource{
		URI:         "gatt://models",
		Name:        "models",
		Title:       "Data Models",
		Description: "All detected ORM and sqlc models, tables, fields, and associations.",
		MIMEType:    "text/plain",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		note := s.autoRefreshCodebase(ctx)
		e, err := s.requireEngine()
		if err != nil {
			return nil, err
		}
		out, err := e.Models("")
		if err != nil {
			return nil, err
		}
		return resText(req.Params.URI, note+out), nil
	})
}
