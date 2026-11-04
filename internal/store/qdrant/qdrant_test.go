package qdrant

import (
	"context"
	"fmt"
	"testing"

	"graphallthethings/internal/store"
	"graphallthethings/internal/store/qdrant/qdranttest"
)

func pt(id, hash string) store.Point {
	return store.Point{NodeID: id, Type: "function", Name: id, Text: "t", Hash: hash, Vector: []float32{1, 0, 0}}
}

func TestUpsertMergesInsteadOfRecreating(t *testing.T) {
	srv := qdranttest.New()
	defer srv.Close()
	c := New(srv.URL, "gatt")
	ctx := context.Background()

	if err := c.Upsert(ctx, []store.Point{pt("a", "h1"), pt("b", "h1"), pt("c", "h1")}); err != nil {
		t.Fatal(err)
	}
	// A partial upsert of ONE changed node must leave the other points alone.
	if err := c.Upsert(ctx, []store.Point{pt("b", "h2")}); err != nil {
		t.Fatal(err)
	}
	got := srv.Points("gatt")
	if len(got) != 3 || got["b"]["hash"] != "h2" || got["a"]["hash"] != "h1" {
		t.Errorf("after partial upsert: %d points, b=%v a=%v (want 3, h2, h1)", len(got), got["b"]["hash"], got["a"]["hash"])
	}
	if srv.Recreates != 0 {
		t.Errorf("collection was recreated %d times by Upsert, want 0", srv.Recreates)
	}
}

func TestReplaceRecreates(t *testing.T) {
	srv := qdranttest.New()
	defer srv.Close()
	c := New(srv.URL, "gatt")
	ctx := context.Background()
	c.Upsert(ctx, []store.Point{pt("a", "h"), pt("b", "h")})
	if err := c.Replace(ctx, []store.Point{pt("a", "h")}); err != nil {
		t.Fatal(err)
	}
	if got := srv.Points("gatt"); len(got) != 1 {
		t.Errorf("Replace left %d points, want 1", len(got))
	}
}

func TestHashesPagesThroughEverythingAndDeleteWorks(t *testing.T) {
	srv := qdranttest.New()
	defer srv.Close()
	c := New(srv.URL, "gatt")
	ctx := context.Background()

	if h, err := c.Hashes(ctx); err != nil || len(h) != 0 {
		t.Fatalf("missing collection: hashes=%v err=%v, want empty and no error", h, err)
	}
	var pts []store.Point
	for i := 0; i < 10; i++ { // page size in the fake is 3
		pts = append(pts, pt(fmt.Sprintf("n%d", i), fmt.Sprintf("h%d", i)))
	}
	c.Upsert(ctx, pts)
	h, err := c.Hashes(ctx)
	if err != nil || len(h) != 10 || h["n7"] != "h7" {
		t.Fatalf("hashes: %d entries, n7=%q, err=%v (want 10, h7)", len(h), h["n7"], err)
	}
	if err := c.Delete(ctx, []string{"n0", "n9"}); err != nil {
		t.Fatal(err)
	}
	if got := srv.Points("gatt"); len(got) != 8 || got["n0"] != nil {
		t.Errorf("after delete: %d points, n0 still there=%v", len(got), got["n0"] != nil)
	}
}
