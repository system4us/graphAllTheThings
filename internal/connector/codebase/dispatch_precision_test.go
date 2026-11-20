package codebase

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"graphallthethings/internal/graph"
)

// Regressions from a real Go+TS repo where 75% of dispatch edges were wrong:
// constants resolved by bare name across packages, data maps read as handler
// tables, switch cases linked from any call, and a JS Promise `reject`
// callback resolved to a Go method of the same name.
func TestDispatchPrecision(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		// A distinctive key really registered somewhere…
		"tokens/table.go": `package tokens

func hexHandler() {}

var table = map[string]func(){"0123456789abcdef": hexHandler}
`,
		// …and a test-local const with that value in another package…
		"audit/export_test.go": `package audit

import "testing"

func TestExport(t *testing.T) {
	const d = "0123456789abcdef"
	_ = d
}
`,
		// …must not make a local variable d anywhere else a trigger.
		"config/scope.go": `package config

import "strings"

func Matches(a, b string) bool { return a == b }

func ProviderDenied(d string) bool {
	return Matches(strings.ToLower(d), "x")
}
`,
		// A data map whose value happens to share a function's name.
		"report/label.go": `package report

func label() string { return "" }

func row(label string) map[string]string {
	return map[string]string{"label": label}
}

func lookupLabel(m map[string]string) string { return get("label") }

func get(k string) string { return k }
`,
		// Switch dispatch: only callers of Parse (or emitters) reach the branch.
		"adapter/adapter.go": `package adapter

import "strings"

func parseChat() {}

func Parse(format string) {
	switch format {
	case "openai.chat":
		parseChat()
	}
}

func TestParse() { Parse("openai.chat") }

func unrelated() bool { return strings.HasPrefix("openai.chat", "x") }
`,
		// Registration with a generic key: only an emit-style call links.
		"bus/bus.go": `package bus

import "fmt"

type Bus struct{}

func (b *Bus) Subscribe(k string, f func()) {}
func (b *Bus) Emit(k string)                {}

func onDone() {}

func setup(b *Bus) { b.Subscribe("done", onDone) }

func logDone() { fmt.Println("done") }

func finish(b *Bus) { b.Emit("done") }
`,
		// Qualified Go constant shared by emitter and listener packages.
		"events/events.go": `package events

const UserCreated = "user.created"
`,
		"listen/listen.go": `package listen

import "example.com/m/events"

type Bus interface{ Subscribe(string, func()) }

func onCreated() {}

func wire(b Bus) { b.Subscribe(events.UserCreated, onCreated) }
`,
		"api/api.go": `package api

import "example.com/m/events"

type Bus interface{ Emit(string) }

func createUser(b Bus) { b.Emit(events.UserCreated) }
`,
		// Cross-language: a Promise reject callback is not Go's reject.
		"proxy/proxy.go": `package proxy

type Server struct{}

func (s *Server) reject() {}

func chat(emit func(string)) { emit("error") }
`,
		"web/gateway.ts": `
export function start(proc: any) {
	return new Promise((resolve, reject) => {
		proc.on("error", reject);
	});
}
`,
	}
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	g, err := New(dir).Extract(context.Background())
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	edge := func(from, to string) bool {
		for _, e := range g.Edges {
			if e.Type != graph.EdgeCalls || e.Attrs["via"] != "dispatch" {
				continue
			}
			fn, tn := g.Nodes[e.From], g.Nodes[e.To]
			if fn != nil && tn != nil && fn.Name == from && tn.Name == to {
				return true
			}
		}
		return false
	}

	wrong := [][2]string{
		{"ProviderDenied", "hexHandler"}, // local var d borrowed a test const's value
		{"lookupLabel", "label"},         // map[string]string is data, not a handler table
		{"unrelated", "parseChat"},       // a switch case linked from an unrelated call
		{"logDone", "onDone"},            // generic key through a non-emit call
		{"chat", "reject"},               // JS reject callback resolved to a Go method
	}
	for _, w := range wrong {
		if edge(w[0], w[1]) {
			t.Errorf("unexpected dispatch edge %s -> %s", w[0], w[1])
		}
	}
	right := [][2]string{
		{"TestParse", "parseChat"},  // caller of the function holding the switch
		{"finish", "onDone"},        // emit-style verb with a generic key
		{"createUser", "onCreated"}, // pkg-qualified const on both sides
	}
	for _, r := range right {
		if !edge(r[0], r[1]) {
			t.Errorf("missing dispatch edge %s -> %s", r[0], r[1])
		}
	}
}
