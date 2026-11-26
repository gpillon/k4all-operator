package hooks

// Waiting is what a hook's Sync or PostInstall returns, as its error, when
// its component is installed as asked but the hook's own work waits on the
// cluster, not on a fault: Rook on fewer than 3 nodes, whose CephCluster k4all
// creates once a third node has joined. The reconciler shows the component as
// "waiting: <Reason>" instead of a hook failure and keeps the ReleaseManifest
// Ready for it (the feature's own condition says what is missing); the hook
// runs again at the next reconcile, and the first run that does not wait
// clears the message. It may be wrapped (fmt.Errorf with %w).
type Waiting struct {
	// Reason says what the hook waits for, for the component's status.
	Reason string
}

func (w Waiting) Error() string { return "waiting: " + w.Reason }
