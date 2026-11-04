package local

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"graphallthethings/internal/store"
)

func point(id string, v ...float32) store.Point {
	return store.Point{NodeID: id, Type: "function", Name: id, Text: id, Vector: v}
}

func TestRoundTripSplitFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vectors.json")
	s := New(path)
	s.Model = "m"
	if err := s.Upsert(context.Background(), []store.Point{point("a", 3, 4), point("b", 0, 2)}); err != nil {
		t.Fatal(err)
	}
	// The metadata file must not carry vectors any more (that was the 106 MB).
	raw, _ := os.ReadFile(path)
	var f file
	json.Unmarshal(raw, &f)
	if f.Format != splitFormat || f.Entries[0].Vector != nil {
		t.Errorf("format=%q, inline vector=%v; want split format with no inline vectors", f.Format, f.Entries[0].Vector)
	}
	got := New(path).Indexed()
	// Vectors are normalized on write: (3,4) -> (0.6,0.8).
	if v := got["a"].Vector; math.Abs(float64(v[0])-0.6) > 1e-6 || math.Abs(float64(v[1])-0.8) > 1e-6 {
		t.Errorf("round trip a = %v, want [0.6 0.8]", v)
	}
	if v := got["b"].Vector; v[0] != 0 || v[1] != 1 {
		t.Errorf("round trip b = %v, want [0 1]", v)
	}
	if PeekModel(path) != "m" {
		t.Errorf("PeekModel = %q, want m", PeekModel(path))
	}
}

// Files written by older gatt (vectors inline in the JSON) must still load, and
// the next write upgrades them.
func TestLegacyInlineFileStillLoadsAndUpgrades(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vectors.json")
	legacy := `{"model":"old","dim":2,"entries":[{"id":"a","type":"function","name":"a","h":"x","vector":[1,0]}]}`
	os.WriteFile(path, []byte(legacy), 0o644)

	s := New(path)
	if got := s.Indexed(); len(got) != 1 || got["a"].Vector[0] != 1 {
		t.Fatalf("legacy load: %v", got)
	}
	s.Model = "old"
	if err := s.Upsert(context.Background(), []store.Point{point("b", 0, 1)}); err != nil {
		t.Fatal(err)
	}
	got := New(path).Indexed()
	if len(got) != 2 || got["a"].Vector[0] != 1 || got["b"].Vector[1] != 1 {
		t.Errorf("after upgrade: %v", got)
	}
	if _, err := os.Stat(binPath(path)); err != nil {
		t.Error("upgrade did not write vectors.bin")
	}
}

func TestMismatchedBlobIsAnErrorNotGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vectors.json")
	s := New(path)
	s.Model = "m"
	s.Upsert(context.Background(), []store.Point{point("a", 1, 0), point("b", 0, 1)})

	// A blob from some other write (wrong generation): must not be read as ours.
	blob, _ := os.ReadFile(binPath(path))
	copy(blob[:16], "0000000000000000")
	os.WriteFile(binPath(path), blob, 0o644)
	if got := New(path).Indexed(); len(got) != 0 {
		t.Errorf("mismatched blob produced %d vectors, want none", len(got))
	}
	// Truncated blob likewise.
	os.WriteFile(binPath(path), blob[:20], 0o644)
	if got := New(path).Indexed(); len(got) != 0 {
		t.Errorf("truncated blob produced %d vectors, want none", len(got))
	}
}

// A reader running while another process rewrites the index must see either
// the old or the new state, never a mix of the two files.
func TestReaderNeverSeesAMixedWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vectors.json")
	w := New(path)
	w.Model = "m"
	// Generation g encodes g in the vector so a mix of two generations is visible.
	write := func(g int) {
		var pts []store.Point
		for i := 0; i < 20; i++ {
			pts = append(pts, point(fmt.Sprintf("n%d", i), float32(g), 1))
		}
		if err := w.Replace(context.Background(), pts); err != nil {
			t.Error(err)
		}
	}
	write(1)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for g := 2; g < 60; g++ {
			write(g)
		}
		close(stop)
	}()
	bad := 0
	for done := false; !done; {
		select {
		case <-stop:
			done = true
		default:
		}
		got := New(path).Indexed()
		if len(got) == 0 {
			continue // transient mismatch beyond retries is reported as "no index", never garbage
		}
		first := got["n0"].Vector[0]
		for _, c := range got {
			if c.Vector[0] != first {
				bad++
				break
			}
		}
	}
	wg.Wait()
	if bad > 0 {
		t.Errorf("%d reads saw vectors from two different writes", bad)
	}
}
