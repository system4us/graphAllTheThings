// Package lock is a small cross-process advisory lock built on O_EXCL file
// creation, so it works the same on Linux, macOS and Windows without extra
// dependencies. It guards short critical sections (load-merge-write of
// vectors.json, a graph refresh+save) between concurrent gatt processes.
package lock

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

// StaleAfter is how old a lock file must be before it is assumed to belong to
// a crashed process and is taken over. Critical sections are seconds long.
var StaleAfter = 5 * time.Minute

// ErrTimeout is returned when the lock could not be taken in time.
var ErrTimeout = errors.New("lock: timed out waiting for another gatt process")

// Acquire blocks until it creates path exclusively, or timeout elapses. The
// returned release function removes the lock and is safe to call more than once.
func Acquire(path string, timeout time.Duration) (release func(), err error) {
	deadline := time.Now().Add(timeout)
	for wait := 5 * time.Millisecond; ; {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprint(f, strconv.Itoa(os.Getpid()))
			f.Close()
			var once bool
			return func() {
				if !once {
					once = true
					os.Remove(path)
				}
			}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		// Take over a lock left behind by a crashed process.
		if st, serr := os.Stat(path); serr == nil && time.Since(st.ModTime()) > StaleAfter {
			os.Remove(path)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%w (%s)", ErrTimeout, path)
		}
		time.Sleep(wait)
		if wait < 100*time.Millisecond {
			wait *= 2
		}
	}
}
