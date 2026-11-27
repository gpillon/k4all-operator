package hooks

import (
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
)

// kindNotServed tells, from the error of a List, that the cluster does not
// serve the kind: now (no match), or any more (a 404: the client mapped the
// kind before its CRD was deleted, and keeps that mapping until the
// operator restarts). A served kind lists empty, never 404. The uninstall
// guards take it as "nothing of that kind exists" (gpillon/k4all-operator#47):
// a CephCluster, a snapshot or a VM cannot exist without its CRD.
func kindNotServed(err error) bool {
	return meta.IsNoMatchError(err) || apierrors.IsNotFound(err)
}
