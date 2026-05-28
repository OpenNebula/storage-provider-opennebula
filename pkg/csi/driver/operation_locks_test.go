package driver

import (
	"testing"
	"time"
)

func TestOperationLocksSerializeSameKey(t *testing.T) {
	locks := NewOperationLocks()
	firstRelease := locks.Acquire("node:1")

	acquired := make(chan struct{})
	go func() {
		release := locks.Acquire("node:1")
		close(acquired)
		release()
	}()

	select {
	case <-acquired:
		t.Fatal("second acquire should have blocked on the same key")
	case <-time.After(10 * time.Millisecond):
	}

	firstRelease()

	select {
	case <-acquired:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("second acquire did not proceed after release")
	}
}

func TestOperationLocksAllowDifferentKeysConcurrently(t *testing.T) {
	locks := NewOperationLocks()
	held := locks.Acquire("node:1")
	defer held()

	acquired := make(chan struct{})
	go func() {
		release := locks.Acquire("node:2")
		close(acquired)
		release()
	}()

	select {
	case <-acquired:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("different keys should not block each other")
	}
}

func TestOperationLocksDeduplicateAndSortKeys(t *testing.T) {
	locks := NewOperationLocks()
	release := locks.Acquire("volume:1", "node:1", "volume:1")
	release()

	if len(locks.locks) != 0 {
		t.Fatalf("expected all lock refs to be released, got %d", len(locks.locks))
	}
}

func TestOperationLocksTryAcquireSameKey(t *testing.T) {
	locks := NewOperationLocks()
	release := locks.Acquire("node:1")
	defer release()

	tryRelease, ok := locks.TryAcquire("node:1")
	if ok {
		defer tryRelease()
		t.Fatal("try acquire should fail while the same key is held")
	}
}

func TestOperationLocksTryAcquireDifferentKey(t *testing.T) {
	locks := NewOperationLocks()
	release := locks.Acquire("node:1")
	defer release()

	tryRelease, ok := locks.TryAcquire("node:2")
	if !ok {
		t.Fatal("try acquire should succeed for a different key")
	}
	tryRelease()
}
