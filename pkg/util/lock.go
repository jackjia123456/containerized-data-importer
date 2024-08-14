package util

import (
	"k8s.io/apimachinery/pkg/util/sets"
	"sync"
)

// ResourceLocks implements a map with atomic operations. It stores a set of all volume IDs
// with an ongoing operation.
type ResourceLocks struct {
	locks sets.Set[string]
	mux   sync.Mutex
}

// NewResourceLocks returns new ResourceLocks.
func NewResourceLocks() *ResourceLocks {
	return &ResourceLocks{
		locks: sets.New[string](),
	}
}

// TryAcquire tries to acquire the lock for operating on volumeID and returns true if successful.
// If another operation is already using volumeID, returns false.
func (vl *ResourceLocks) TryAcquire(resource string) bool {
	vl.mux.Lock()
	defer vl.mux.Unlock()
	if vl.locks.Has(resource) {
		return false
	}
	vl.locks.Insert(resource)

	return true
}

// Release deletes the lock on volumeID.
func (vl *ResourceLocks) Release(resource string) {
	vl.mux.Lock()
	defer vl.mux.Unlock()
	vl.locks.Delete(resource)
}
