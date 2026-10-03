package controller

import (
	"context"
	"sync"
)

// Returns false if ctx ends first. release drains the channel it was given,
// even if the caller's variable is replaced meanwhile.
func AcquireSlot(ctx context.Context, slots chan struct{}) (release func(), ok bool) {
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, true
	case <-ctx.Done():
		return nil, false
	}
}

// Bounds in-flight work per key. Entries are removed once nobody holds or
// waits for them.
type KeyedSlots struct {
	mu       sync.Mutex
	capacity int
	keys     map[int64]*keyedSlot
}

type keyedSlot struct {
	slots chan struct{}
	refs  int
}

func NewKeyedSlots(capacity int) *KeyedSlots {
	return &KeyedSlots{capacity: capacity, keys: make(map[int64]*keyedSlot)}
}

func (k *KeyedSlots) Acquire(ctx context.Context, key int64) (release func(), ok bool) {
	k.mu.Lock()
	entry := k.keys[key]
	if entry == nil {
		entry = &keyedSlot{slots: make(chan struct{}, k.capacity)}
		k.keys[key] = entry
	}
	entry.refs++
	k.mu.Unlock()
	releaseSlot, ok := AcquireSlot(ctx, entry.slots)
	if !ok {
		k.unref(key, entry)
		return nil, false
	}
	return func() {
		releaseSlot()
		k.unref(key, entry)
	}, true
}

func (k *KeyedSlots) unref(key int64, entry *keyedSlot) {
	k.mu.Lock()
	defer k.mu.Unlock()
	entry.refs--
	if entry.refs == 0 {
		delete(k.keys, key)
	}
}

func (k *KeyedSlots) Len() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.keys)
}
