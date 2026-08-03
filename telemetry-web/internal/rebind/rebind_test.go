package rebind

import (
	"sync"
	"sync/atomic"
	"testing"
)

// Registrations are process-wide and there is no unregister — by design, since
// real hooks are installed once from package init. Tests therefore clear the
// list so one test's hook is not re-run by another's Notify.
func isolate(t *testing.T) {
	t.Helper()
	mu.Lock()
	saved := fns
	fns = nil
	mu.Unlock()

	t.Cleanup(func() {
		mu.Lock()
		fns = saved
		mu.Unlock()
	})
}

func TestNotifyRunsEveryRegisteredHook(t *testing.T) {
	isolate(t)

	var a, b int
	On(func() { a++ })
	On(func() { b++ })

	Notify()
	if a != 1 || b != 1 {
		t.Fatalf("after one Notify: a=%d b=%d, want 1 and 1", a, b)
	}

	Notify()
	if a != 2 || b != 2 {
		t.Errorf("after two Notifies: a=%d b=%d, want 2 and 2", a, b)
	}
}

func TestNotifyWithNoHooksIsANoop(t *testing.T) {
	isolate(t)
	Notify() // must not panic or deadlock
}

// Notify must not hold the lock while running hooks, or a hook that registers
// another one would deadlock.
func TestHookMayRegisterDuringNotify(t *testing.T) {
	isolate(t)

	var nested bool
	On(func() {
		if !nested {
			nested = true
			On(func() {})
		}
	})

	Notify()
	if !nested {
		t.Fatal("hook did not run")
	}
}

// Registration happens from package init and Notify from Init; both are
// exercised under -race to keep the list safe if that ever overlaps.
func TestConcurrentRegisterAndNotify(t *testing.T) {
	isolate(t)

	var runs atomic.Int64
	On(func() { runs.Add(1) })

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(2)
		go func() { defer wg.Done(); On(func() { runs.Add(1) }) }()
		go func() { defer wg.Done(); Notify() }()
	}
	wg.Wait()

	if runs.Load() == 0 {
		t.Error("no hook ran across 8 concurrent Notify calls")
	}
}

// A panicking hook must not abort the remaining rebuilds or unwind out of
// telemetry.Init — a telemetry-wiring bug should never crash startup.
func TestPanickingHookIsIsolated(t *testing.T) {
	isolate(t)

	var before, after bool
	On(func() { before = true })
	On(func() { panic("hook exploded") })
	On(func() { after = true })

	Notify() // must not propagate

	if !before {
		t.Error("hook registered before the panicking one did not run")
	}
	if !after {
		t.Error("hook registered after the panicking one was skipped")
	}
}
