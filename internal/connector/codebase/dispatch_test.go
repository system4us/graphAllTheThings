package codebase

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/system4us/graphAllTheThings/pkg/graph"
)

func TestDispatchFanOutConstantsInlineSwitchAndGo(t *testing.T) {
	dir := t.TempDir()

	files := map[string]string{
		"events.ts": `
export const Events = {
	ORDER_PAID: "order.paid",
};
export const EVT_SHIPPED = "order.shipped";

export function sendReceipt(e: any) { return e; }
export function updateStats(e: any) { return e; }
export function notifyWarehouse(e: any) { return e; }
export function handleRefund(e: any) { return e; }

export function registerListeners(bus: any) {
	bus.on(Events.ORDER_PAID, sendReceipt);
	bus.on("order.paid", (evt: any) => { updateStats(evt); });
	bus.on(EVT_SHIPPED, notifyWarehouse);
}

export function routeEvent(evt: any) {
	switch (evt.type) {
		case "order.refunded":
			return handleRefund(evt);
	}
}

export function checkout(bus: any) {
	bus.emit(Events.ORDER_PAID, { id: 1 });
	bus.emit(EVT_SHIPPED, { id: 1 });
	bus.emit("order.refunded", { id: 1 });
}
`,
		"bus.go": `package app

const EvtUserCreated = "user.created"

func onUserCreated() {}
func onUserCreatedAudit() {}

func setupBus(bus interface{ Subscribe(string, func()) }) {
	bus.Subscribe(EvtUserCreated, onUserCreated)
	bus.Subscribe("user.created", onUserCreatedAudit)
}

func createUser(bus interface{ Emit(string) }) {
	bus.Emit(EvtUserCreated)
}
`,
	}

	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	g, err := New(dir).Extract(context.Background())
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	hasDispatchEdge := func(fromName, toName, wantEvent string) bool {
		for _, e := range g.Edges {
			if e.Type != graph.EdgeCalls || e.Attrs["via"] != "dispatch" {
				continue
			}
			fn := g.Nodes[e.From]
			tn := g.Nodes[e.To]
			if fn != nil && tn != nil && fn.Name == fromName && tn.Name == toName {
				if wantEvent == "" || e.Attrs["event"] == wantEvent {
					return true
				}
			}
		}
		return false
	}

	// TS fan-out: both sendReceipt (via Events.ORDER_PAID) and updateStats (via inline arrow)
	if !hasDispatchEdge("checkout", "sendReceipt", "order.paid") {
		t.Errorf("expected dispatch edge checkout -> sendReceipt (event=order.paid)")
	}
	if !hasDispatchEdge("checkout", "updateStats", "order.paid") {
		t.Errorf("expected dispatch edge checkout -> updateStats (event=order.paid, inline callback fan-out)")
	}
	// TS const string
	if !hasDispatchEdge("checkout", "notifyWarehouse", "order.shipped") {
		t.Errorf("expected dispatch edge checkout -> notifyWarehouse (event=order.shipped)")
	}
	// TS switch/case
	if !hasDispatchEdge("checkout", "handleRefund", "order.refunded") {
		t.Errorf("expected dispatch edge checkout -> handleRefund (event=order.refunded, switch/case)")
	}
	// Go fan-out + const
	if !hasDispatchEdge("createUser", "onUserCreated", "user.created") {
		t.Errorf("expected Go dispatch edge createUser -> onUserCreated (event=user.created)")
	}
	if !hasDispatchEdge("createUser", "onUserCreatedAudit", "user.created") {
		t.Errorf("expected Go dispatch edge createUser -> onUserCreatedAudit (event=user.created)")
	}
}
