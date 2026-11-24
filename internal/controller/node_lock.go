package controller

import "sync"

// NodeLock is shared by the controllers that change nodes one at a time: the
// ClusterUpgrade and the node-settings rollout. Neither starts a node while
// the other has one in its step (they read that from the Node annotations),
// and each holds the lock for a whole reconcile, reading the Nodes from the
// API server, not the cache: the other's last write is always seen.
//
// It also remembers that the operator evicted its own pod (k4all#97): the
// process is about to end, and its last reconciles must start nothing that
// the next leader would not know about.
type NodeLock struct {
	mu      sync.Mutex
	leaving bool
}

// lock takes the lock and returns its release; a nil lock locks nothing.
func (l *NodeLock) lock() func() {
	if l == nil {
		return func() {}
	}
	l.mu.Lock()
	return l.mu.Unlock
}

// leave records that the operator evicted its own pod. Call with the lock
// held.
func (l *NodeLock) leave() {
	if l != nil {
		l.leaving = true
	}
}

// left tells whether the operator evicted its own pod. Call with the lock
// held.
func (l *NodeLock) left() bool {
	return l != nil && l.leaving
}
