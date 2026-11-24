package controller

import (
	"strings"
	"time"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

// retryBackoff are the waits between the tries of a failed component: the
// first 30 s after it failed, then 1, 2 and 4 minutes after the one before,
// then retryBackoffCap. A component that failed is tried again at every
// reconcile; the backoff sets how soon the reconcile comes back for it. It
// used to wait for the 5-minute resync: Calico failing on DNS at first boot
// left a cluster waiting 13 minutes.
var retryBackoff = []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute}

const retryBackoffCap = 5 * time.Minute

// retryAfter is how soon a component that has failed for failedFor (since
// its Failed lastTransitionTime, or since it was installed for a failed
// hook) is tried again: the tries fall at fixed times from the failure
// (30 s, 1m30, 3m30, 7m30, then every 5 minutes), so nothing but the status
// says where the backoff is, and a reconcile in between does not move them.
func retryAfter(failedFor time.Duration) time.Duration {
	failedFor = max(failedFor, 0)
	var at time.Duration
	for _, wait := range retryBackoff {
		at += wait
		if failedFor < at {
			return at - failedFor
		}
	}
	return retryBackoffCap - (failedFor-at)%retryBackoffCap
}

// retried tells whether a component's status is a failure that the
// reconcile tries again with retryBackoff: Failed, whatever the error (an
// apply, an uninstall, a stuck operator uninstall), or installed with a
// failed hook. A refused downgrade waits on a person, not on a retry.
func retried(st k4allv1alpha1.ComponentStatus) bool {
	if st.State == k4allv1alpha1.ComponentStateFailed {
		return !strings.HasPrefix(st.Message, downgradeRefusedPrefix)
	}
	return strings.HasPrefix(st.Message, hookFailedPrefix)
}

// failedRetry is how soon the reconcile comes back to try the failed
// components of the release again (retryAfter), 0 when none failed.
func failedRetry(rm *k4allv1alpha1.ReleaseManifest, now time.Time) time.Duration {
	var next time.Duration
	for name, st := range rm.Status.Components {
		if _, inRelease := rm.Spec.Components[name]; !inRelease || !retried(st) {
			continue
		}
		next = sooner(next, retryAfter(now.Sub(st.LastTransitionTime.Time)))
	}
	return next
}

// keepFailedSince gives every component that was Failed before this
// reconcile and still is the lastTransitionTime it had: the time it went
// Failed, which retryAfter counts from. Each try writes a new status, and its
// time would restart the backoff (and, a new status at every try, start the
// next reconcile at once).
func keepFailedSince(before map[string]k4allv1alpha1.ComponentStatus, rm *k4allv1alpha1.ReleaseManifest) {
	for name, st := range rm.Status.Components {
		prev, ok := before[name]
		if !ok || st.State != k4allv1alpha1.ComponentStateFailed || prev.State != k4allv1alpha1.ComponentStateFailed ||
			prev.LastTransitionTime.IsZero() {
			continue
		}
		st.LastTransitionTime = prev.LastTransitionTime
		rm.Status.Components[name] = st
	}
}
