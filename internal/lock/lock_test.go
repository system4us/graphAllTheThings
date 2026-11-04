package lock

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestMutualExclusion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	var inside, maxInside int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rel, err := Acquire(path, 5*time.Second)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			inside++
			if inside > maxInside {
				maxInside = inside
			}
			mu.Unlock()
			time.Sleep(5 * time.Millisecond)
			mu.Lock()
			inside--
			mu.Unlock()
			rel()
		}()
	}
	wg.Wait()
	if maxInside != 1 {
		t.Errorf("%d holders at once, want 1", maxInside)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("lock file left behind")
	}
}

func TestTimeoutAndStaleTakeover(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	rel, _ := Acquire(path, time.Second)
	if _, err := Acquire(path, 50*time.Millisecond); !errors.Is(err, ErrTimeout) {
		t.Errorf("second Acquire err = %v, want ErrTimeout", err)
	}
	rel()
	rel() // idempotent

	// A lock left by a crashed process (old mtime) is taken over.
	os.WriteFile(path, []byte("99999"), 0o644)
	old := time.Now().Add(-2 * StaleAfter)
	os.Chtimes(path, old, old)
	rel2, err := Acquire(path, time.Second)
	if err != nil {
		t.Fatalf("stale lock not taken over: %v", err)
	}
	rel2()
}
