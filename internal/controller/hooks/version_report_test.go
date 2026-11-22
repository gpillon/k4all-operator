package hooks

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/gpillon/k4all-operator/internal/engine"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestVirtHook_NoKubeVirtCRIsNotInstalled(t *testing.T) {
	h := NewVirtHook(fake.NewClientBuilder().Build(), logr.Discard())

	version, ready, err := h.RunningVersion(context.Background())

	if err != nil || version != "" || ready {
		t.Fatalf("got (%q, %t, %v), want (\"\", false, nil): KubeVirt is not installed", version, ready, err)
	}
}

// kubeVirtCR is the KubeVirt CR as virt-operator reports it: observed is the
// version it runs, target the one it is rolling out.
func kubeVirtCR(observed, target string, conditions map[string]string) client.Object {
	cr := &unstructured.Unstructured{}
	cr.SetAPIVersion("kubevirt.io/v1")
	cr.SetKind("KubeVirt")
	cr.SetNamespace("kubevirt")
	cr.SetName("kubevirt")
	conds := make([]interface{}, 0, len(conditions))
	for typ, status := range conditions {
		conds = append(conds, map[string]interface{}{"type": typ, "status": status})
	}
	cr.Object["status"] = map[string]interface{}{
		"observedKubeVirtVersion": observed,
		"targetKubeVirtVersion":   target,
		"phase":                   "Deployed",
		"conditions":              conds,
	}
	return cr
}

func TestVirtHook_ReportsTheVersionVirtOperatorRuns(t *testing.T) {
	cr := kubeVirtCR("v1.7.0", "v1.7.0", map[string]string{"Available": "True", "Progressing": "False", "Degraded": "False"})
	h := NewVirtHook(fake.NewClientBuilder().WithObjects(cr).Build(), logr.Discard())

	version, ready, err := h.RunningVersion(context.Background())

	if err != nil || version != "v1.7.0" || !ready {
		t.Fatalf("got (%q, %t, %v), want (\"v1.7.0\", true, nil)", version, ready, err)
	}
}

func TestVirtHook_IsNotReadyWhileAVersionRollsOut(t *testing.T) {
	cr := kubeVirtCR("v1.7.0", "v1.7.4", map[string]string{"Available": "True", "Progressing": "True", "Degraded": "False"})
	h := NewVirtHook(fake.NewClientBuilder().WithObjects(cr).Build(), logr.Discard())

	version, ready, err := h.RunningVersion(context.Background())

	if err != nil || version != "v1.7.0" || ready {
		t.Fatalf("got (%q, %t, %v), want (\"v1.7.0\", false, nil): v1.7.4 is rolling out", version, ready, err)
	}
}

// Before KubeVirt is ever installed its CRD does not exist either.
func TestVirtHook_NoKubeVirtCRDIsNotInstalled(t *testing.T) {
	noCRD := interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
		return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "kubevirt.io", Kind: "KubeVirt"}}
	}}
	h := NewVirtHook(fake.NewClientBuilder().WithInterceptorFuncs(noCRD).Build(), logr.Discard())

	version, ready, err := h.RunningVersion(context.Background())

	if err != nil || version != "" || ready {
		t.Fatalf("got (%q, %t, %v), want (\"\", false, nil): KubeVirt is not installed", version, ready, err)
	}
}

// The reconciler finds the reporters through the registry.
func TestRegistry_KubeVirtAndCDIReportTheirVersion(t *testing.T) {
	cdi := &unstructured.Unstructured{}
	cdi.SetAPIVersion("cdi.kubevirt.io/v1beta1")
	cdi.SetKind("CDI")
	cdi.SetName("cdi")
	cdi.Object["status"] = map[string]interface{}{
		"observedVersion": "v1.64.0", "targetVersion": "v1.64.0", "phase": "Deployed",
		"conditions": []interface{}{map[string]interface{}{"type": "Available", "status": "True"}},
	}
	kv := kubeVirtCR("v1.7.0", "v1.7.0", map[string]string{"Available": "True"})
	reg := NewRegistry(fake.NewClientBuilder().WithObjects(cdi, kv).Build(), logr.Discard())

	for name, want := range map[string]string{"cdi": "v1.64.0", "kubevirt": "v1.7.0"} {
		reporter, ok := reg.Get(name).(engine.VersionReporter)
		if !ok {
			t.Errorf("%s: no VersionReporter hook", name)
			continue
		}
		version, ready, err := reporter.RunningVersion(context.Background())
		if err != nil || version != want || !ready {
			t.Errorf("%s: got (%q, %t, %v), want (%q, true, nil)", name, version, ready, err, want)
		}
	}
}

// A CR virt-operator has not reported on (it is scaled to 0, or has not run
// yet) says nothing of what runs: that is not "not installed".
func TestVirtHook_ACRWithoutAnObservedVersionCannotTell(t *testing.T) {
	cr := kubeVirtCR("", "", nil)
	h := NewVirtHook(fake.NewClientBuilder().WithObjects(cr).Build(), logr.Discard())

	version, _, err := h.RunningVersion(context.Background())

	if err == nil {
		t.Fatalf("got version %q and no error, want an error: the CR reports no version", version)
	}
}
