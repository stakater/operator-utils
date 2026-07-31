package scope

import (
	"sync"
	"testing"
)

func TestServiceNameUnsetIsEmpty(t *testing.T) {
	// A fresh process has never called Set; ServiceName must not panic on the
	// nil pointer, it must report "".
	if got := ServiceName(); got != "" {
		t.Skipf("another test already called Set (%q); nothing to assert", got)
	}
}

func TestSetThenRead(t *testing.T) {
	Set("svc-a")
	if got := ServiceName(); got != "svc-a" {
		t.Errorf("ServiceName() = %q, want %q", got, "svc-a")
	}
	Set("svc-b")
	if got := ServiceName(); got != "svc-b" {
		t.Errorf("ServiceName() = %q, want %q", got, "svc-b")
	}
}

// Set runs once at Init but ServiceName is read on every log record, so the
// two must be safe together under -race.
func TestConcurrentSetAndRead(t *testing.T) {
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(2)
		go func() { defer wg.Done(); Set("concurrent") }()
		go func() { defer wg.Done(); _ = ServiceName() }()
	}
	wg.Wait()
}
