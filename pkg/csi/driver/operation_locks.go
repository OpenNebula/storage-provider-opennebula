package driver

import (
	"sort"
	"sync"
)

type OperationLocks struct {
	mu    sync.Mutex
	locks map[string]*operationLockRef
}

type operationLockRef struct {
	mu   sync.Mutex
	refs int
}

func NewOperationLocks() *OperationLocks {
	return &OperationLocks{
		locks: make(map[string]*operationLockRef),
	}
}

func (o *OperationLocks) Acquire(keys ...string) func() {
	normalized := normalizeLockKeys(keys)
	if len(normalized) == 0 {
		return func() {}
	}

	refs := make([]*operationLockRef, 0, len(normalized))
	o.mu.Lock()
	for _, key := range normalized {
		ref := o.locks[key]
		if ref == nil {
			ref = &operationLockRef{}
			o.locks[key] = ref
		}
		ref.refs++
		refs = append(refs, ref)
	}
	o.mu.Unlock()

	for _, ref := range refs {
		ref.mu.Lock()
	}

	return func() {
		for i := len(refs) - 1; i >= 0; i-- {
			refs[i].mu.Unlock()
		}

		o.mu.Lock()
		defer o.mu.Unlock()
		for idx, key := range normalized {
			ref := refs[idx]
			ref.refs--
			if ref.refs == 0 {
				delete(o.locks, key)
			}
		}
	}
}

func (o *OperationLocks) TryAcquire(key string) (func(), bool) {
	normalized := normalizeLockKeys([]string{key})
	if len(normalized) == 0 {
		return func() {}, true
	}

	o.mu.Lock()
	ref := o.locks[normalized[0]]
	if ref == nil {
		ref = &operationLockRef{}
		o.locks[normalized[0]] = ref
	}
	ref.refs++
	o.mu.Unlock()

	if !ref.mu.TryLock() {
		o.mu.Lock()
		ref.refs--
		if ref.refs == 0 {
			delete(o.locks, normalized[0])
		}
		o.mu.Unlock()
		return nil, false
	}

	return func() {
		ref.mu.Unlock()
		o.mu.Lock()
		defer o.mu.Unlock()
		ref.refs--
		if ref.refs == 0 {
			delete(o.locks, normalized[0])
		}
	}, true
}

func normalizeLockKeys(keys []string) []string {
	if len(keys) == 0 {
		return nil
	}

	set := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if key == "" {
			continue
		}
		set[key] = struct{}{}
	}

	if len(set) == 0 {
		return nil
	}

	normalized := make([]string, 0, len(set))
	for key := range set {
		normalized = append(normalized, key)
	}
	sort.Strings(normalized)
	return normalized
}
