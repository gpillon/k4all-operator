package hooks

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/gpillon/k4all-operator/internal/operatorscheme"
)

// A CRD deleted after the client mapped its kind: the client keeps the
// mapping until the operator restarts, and every List of the kind gets the
// API server's 404 (#47). The admin deleted Rook's CRDs by hand, then turned
// Rook off; or, in the shared envtest, another test removed them.

// crdKindsGone is a fake cluster with objs whose every List of a kind of an
// API group (a CRD's kind; the core kinds are served) gets that 404.
func crdKindsGone(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	base := fake.NewClientBuilder().WithScheme(operatorscheme.New()).WithObjects(objs...).Build()
	return interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			gvk := list.GetObjectKind().GroupVersionKind()
			if gvk.Group == "" {
				return cl.List(ctx, list, opts...)
			}
			resource := strings.ToLower(strings.TrimSuffix(gvk.Kind, "List")) + "s"
			return apierrors.NewGenericServerResponse(http.StatusNotFound, "get",
				schema.GroupResource{Group: gvk.Group, Resource: resource}, "", "", 0, false)
		},
	})
}

// Every uninstall guard (Rook and its CSI drivers, the snapshot controller,
// KubeVirt, CDI) takes a 404 on its List as a kind the cluster does not
// serve, of which nothing exists, as it takes no match: the uninstall goes
// on. Before, Rook's and the snapshot controller's refused it ("cannot
// tell") until the operator restarted.
func TestUninstallGuards_AKindNoLongerServedHoldsNothing(t *testing.T) {
	reg := NewRegistry(crdKindsGone(t), logr.Discard())
	var guarded []string
	for name, hook := range reg.hooks {
		if _, ok := hook.(UninstallGuard); ok {
			guarded = append(guarded, name)
		}
	}
	sort.Strings(guarded)
	if len(guarded) < 5 {
		t.Fatalf("uninstall guards %v, want at least rook-ceph, ceph-csi-drivers, snapshot-controller, kubevirt, cdi", guarded)
	}
	for _, name := range guarded {
		reason, err := reg.Get(name).(UninstallGuard).UninstallBlocked(context.Background(), name)
		if err != nil || reason != "" {
			t.Errorf("%s: uninstall refused (%q, %v), want it let through: its kinds are not served", name, reason, err)
		}
	}
}

// The kubevirt-manager removal reads where VMs name a PriorityClass: a kind
// no longer served names none, and the unused PriorityClasses go.
func TestKubevirtManagerRemoval_AKindNoLongerServedNamesNoPriorityClass(t *testing.T) {
	c := crdKindsGone(t, priorityClass("vm-standard"), priorityClass("vm-preemptible"))

	removeKubevirtManager(t, c)

	for _, name := range []string{"vm-standard", "vm-preemptible"} {
		if exists(t, c, priorityClass(name)) {
			t.Errorf("PriorityClass %s kept, though no VM kind is served", name)
		}
	}
}
