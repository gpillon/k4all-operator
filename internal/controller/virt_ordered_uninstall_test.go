package controller

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/controller/hooks"
)

// The test plays virt-operator and cdi-operator: each puts its finalizer on
// its CR, and removes it once it has uninstalled what it created.
const (
	kubeVirtFinalizer = "kubevirt.io/foregroundDeleteKubeVirt"
	cdiFinalizer      = "operator.cdi.kubevirt.io"
)

// blockUninstall is the uninstallStrategy that makes the operators refuse to
// delete their CR while workloads exist.
const blockUninstall = "BlockUninstallIfWorkloadsExist"

// runningOperatorCR creates the CR of a running operator: KubeVirt
// kubevirt/kubevirt or CDI cdi, Deployed, with the operator's finalizer. It
// is gone after the test.
func runningOperatorCR(t *testing.T, c client.Client, g gatedCR, finalizer string) {
	t.Helper()
	if g.key.Namespace != "" {
		ensureNamespace(t, c, g.key.Namespace)
	}
	cr := &unstructured.Unstructured{}
	cr.SetGroupVersionKind(g.gvk)
	cr.SetNamespace(g.key.Namespace)
	cr.SetName(g.key.Name)
	cr.SetFinalizers([]string{finalizer})
	cr.Object["spec"] = map[string]interface{}{}
	if err := c.Create(context.Background(), cr); err != nil {
		t.Fatalf("create %s: %v", g.gvk.Kind, err)
	}
	t.Cleanup(func() { g.operatorDone(t, c) })
}

// operatorDone is the operator finishing its uninstall: it removes its
// finalizer, and the CR goes (if it was deleted).
func (g gatedCR) operatorDone(t *testing.T, c client.Client) {
	t.Helper()
	ctx := context.Background()
	cr := &unstructured.Unstructured{}
	cr.SetGroupVersionKind(g.gvk)
	if err := c.Get(ctx, g.key, cr); err != nil {
		if !apierrors.IsNotFound(err) {
			t.Error(err)
		}
		return
	}
	cr.SetFinalizers(nil)
	if err := c.Update(ctx, cr); client.IgnoreNotFound(err) != nil {
		t.Error(err)
	}
	_ = c.Delete(ctx, cr)
}

// deleting tells whether the CR is there, and being deleted.
func (g gatedCR) deleting(t *testing.T, c client.Client) bool {
	t.Helper()
	cr := &unstructured.Unstructured{}
	cr.SetGroupVersionKind(g.gvk)
	if err := c.Get(context.Background(), g.key, cr); err != nil {
		if apierrors.IsNotFound(err) {
			return false
		}
		t.Fatal(err)
	}
	return cr.GetDeletionTimestamp() != nil
}

func readyCondition(t *testing.T, c client.Client) metav1.Condition {
	t.Helper()
	rm := &k4allv1alpha1.ReleaseManifest{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "k4all"}, rm); err != nil {
		t.Fatal(err)
	}
	cond := meta.FindStatusCondition(rm.Status.Conditions, "Ready")
	if cond == nil {
		t.Fatal("no Ready condition")
	}
	return *cond
}

// assertOperatorKept fails the test unless the component's objects (its
// operator) are still there.
func assertOperatorKept(t *testing.T, c client.Client, name string, objects *corev1.ConfigMap) {
	t.Helper()
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(objects), &corev1.ConfigMap{}); err != nil {
		t.Fatalf("%s's objects (its operator): %v, want them kept while its CR is there", name, err)
	}
}

// Turning virt off deletes the KubeVirt CR first and keeps virt-operator
// until it has removed what it created: virt-api, virt-handler, the
// webhooks and the APIServices. Deleting virt-operator first (the engine's
// reverse order) left the CR stuck on its finalizer, and the webhooks and
// APIServices behind, which break namespace deletion in the whole cluster.
// Meanwhile the component is Uninstalling and the release is not Ready.
func TestReconcile_KubeVirtGoesBeforeVirtOperator(t *testing.T) {
	c, r, objects := virtOnEnvtest(t, "kubevirt")
	runningOperatorCR(t, c, kubeVirtCR, kubeVirtFinalizer)

	reconcileProbe(t, r)

	if !kubeVirtCR.deleting(t, c) {
		t.Fatal("the KubeVirt CR is not being deleted")
	}
	assertOperatorKept(t, c, "kubevirt", objects)
	st := componentStatus(t, c, "kubevirt")
	if st.State != k4allv1alpha1.ComponentStateUninstalling || st.InstalledVersion != "v1" ||
		!strings.HasPrefix(st.Message, "waiting for virt-operator to remove KubeVirt kubevirt/kubevirt") {
		t.Fatalf("kubevirt: %s at %q (%s), want Uninstalling at v1, waiting for virt-operator",
			st.State, st.InstalledVersion, st.Message)
	}
	if ready := readyCondition(t, c); ready.Status != metav1.ConditionFalse || ready.Reason != "ComponentsUninstalling" {
		t.Fatalf("Ready %s (%s: %s), want False, ComponentsUninstalling", ready.Status, ready.Reason, ready.Message)
	}

	reconcileProbe(t, r) // virt-operator still at work
	assertOperatorKept(t, c, "kubevirt", objects)

	kubeVirtCR.operatorDone(t, c)
	reconcileProbe(t, r)

	assertUninstalled(t, c, "kubevirt", objects)
}

// The KubeVirt and CDI CRs are deleted only with spec.uninstallStrategy
// BlockUninstallIfWorkloadsExist, which k4all's CRs did not have: unset,
// virt-operator deletes the VMs with its CRDs, and the garbage collector
// deletes CDI's CRDs, the DataVolumes and their PVCs with them, once the CDI
// CR is gone. The test sees every delete the operator sends.
func TestReconcile_TheOperatorCRIsDeletedWithBlockUninstallIfWorkloadsExist(t *testing.T) {
	for _, tc := range []struct {
		component, finalizer string
		cr                   gatedCR
	}{
		{"kubevirt", kubeVirtFinalizer, kubeVirtCR},
		{"cdi", cdiFinalizer, cdiCR},
	} {
		t.Run(tc.component, func(t *testing.T) {
			c, r, _ := virtOnEnvtest(t, tc.component)
			runningOperatorCR(t, c, tc.cr, tc.finalizer)
			watching, err := client.NewWithWatch(envtestConfig(t), client.Options{Scheme: gateScheme(t)})
			if err != nil {
				t.Fatal(err)
			}
			deletes := 0
			r.Client = interceptor.NewClient(watching, interceptor.Funcs{
				Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					if obj.GetObjectKind().GroupVersionKind().Kind == tc.cr.gvk.Kind {
						deletes++
						strategy, _, _ := unstructured.NestedString(tc.cr.get(t, c).Object, "spec", "uninstallStrategy")
						if strategy != blockUninstall {
							t.Errorf("%s deleted with uninstallStrategy %q, want BlockUninstallIfWorkloadsExist",
								tc.cr.gvk.Kind, strategy)
						}
					}
					return cl.Delete(ctx, obj, opts...)
				},
			})

			reconcileProbe(t, r)

			if deletes != 1 || !tc.cr.deleting(t, c) {
				t.Fatalf("%s: %d deletes, deleting %v; want it deleted once", tc.cr.gvk.Kind, deletes, tc.cr.deleting(t, c))
			}
		})
	}
}

// An operator that does not finish its uninstall within 15 minutes leaves
// its component Failed, saying what is stuck and where the recovery is, and
// still running: deleting it would strand its CR, webhooks and APIServices.
// The component stays Failed (it does not go back to Uninstalling) until the
// operator is done; then the uninstall goes on.
func TestReconcile_AStuckOperatorUninstallFailsAndKeepsTheOperator(t *testing.T) {
	spec := k4allv1alpha1.ComponentSpec{
		Type: k4allv1alpha1.ComponentTypeManifest, Version: "v1", InlineManifests: []string{guardedConfigMap},
		FeatureGate: "virt",
	}
	c, r := componentOnEnvtest(t, "kubevirt", spec, k4allv1alpha1.ComponentStatus{
		State: k4allv1alpha1.ComponentStateUninstalling, InstalledVersion: "v1",
		Message:            "waiting for virt-operator to remove KubeVirt kubevirt/kubevirt: deleted",
		LastTransitionTime: metav1.NewTime(time.Now().Add(-16 * time.Minute)),
	})
	objects := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "guarded", Namespace: "default"}}
	createAll(t, c, objects)
	runningOperatorCR(t, c, kubeVirtCR, kubeVirtFinalizer)
	cr := kubeVirtCR.get(t, c)
	if err := c.Delete(context.Background(), cr); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		reconcileProbe(t, r)

		assertOperatorKept(t, c, "kubevirt", objects)
		st := componentStatus(t, c, "kubevirt")
		if st.State != k4allv1alpha1.ComponentStateFailed || st.InstalledVersion != "v1" ||
			!strings.HasPrefix(st.Message, "uninstall stuck: waiting for virt-operator to remove KubeVirt kubevirt/kubevirt") ||
			!strings.Contains(st.Message, "https://github.com/gpillon/k4all/issues/94") {
			t.Fatalf("kubevirt: %s at %q (%s), want Failed at v1, stuck, with the recovery link",
				st.State, st.InstalledVersion, st.Message)
		}
	}

	kubeVirtCR.operatorDone(t, c)
	reconcileProbe(t, r)

	assertUninstalled(t, c, "kubevirt", objects)
}

// CDI goes before cdi-operator too, and the uninstall also waits for what
// the CDI CR owned: cdi-operator only stops its workers and removes its
// finalizer, the garbage collector deletes its CRDs, webhooks and
// APIService after the CR (there is none in envtest: the test deletes the
// webhook as it would).
func TestReconcile_CDIGoesBeforeCDIOperatorAndWaitsForWhatItOwned(t *testing.T) {
	c, r, objects := virtOnEnvtest(t, "cdi")
	runningOperatorCR(t, c, cdiCR, cdiFinalizer)
	ignore, none := admissionregistrationv1.Ignore, admissionregistrationv1.SideEffectClassNone
	webhook := &admissionregistrationv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "cdi-api-datavolume-validate", Labels: map[string]string{"cdi.kubevirt.io": "cdi-api"}},
		Webhooks: []admissionregistrationv1.ValidatingWebhook{{
			Name: "datavolume-validate.cdi.kubevirt.io",
			ClientConfig: admissionregistrationv1.WebhookClientConfig{
				Service: &admissionregistrationv1.ServiceReference{Namespace: "cdi", Name: "cdi-api"},
			},
			Rules: []admissionregistrationv1.RuleWithOperations{{
				Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create},
				Rule: admissionregistrationv1.Rule{
					APIGroups: []string{"cdi.kubevirt.io"}, APIVersions: []string{"v1beta1"}, Resources: []string{"datavolumes"},
				},
			}},
			FailurePolicy: &ignore, SideEffects: &none, AdmissionReviewVersions: []string{"v1"},
		}},
	}
	createAll(t, c, webhook)

	reconcileProbe(t, r)

	if !cdiCR.deleting(t, c) {
		t.Fatal("the CDI CR is not being deleted")
	}
	assertOperatorKept(t, c, "cdi", objects)
	if st := componentStatus(t, c, "cdi"); st.State != k4allv1alpha1.ComponentStateUninstalling ||
		!strings.HasPrefix(st.Message, "waiting for cdi-operator to remove CDI cdi") {
		t.Fatalf("cdi: %s (%s), want Uninstalling, waiting for cdi-operator", st.State, st.Message)
	}

	cdiCR.operatorDone(t, c)
	reconcileProbe(t, r)

	assertOperatorKept(t, c, "cdi", objects)
	want := "waiting for the garbage collector to remove what CDI cdi created: " +
		"ValidatingWebhookConfiguration cdi-api-datavolume-validate"
	if st := componentStatus(t, c, "cdi"); st.State != k4allv1alpha1.ComponentStateUninstalling || st.Message != want {
		t.Fatalf("cdi: %s (%s), want Uninstalling (%s)", st.State, st.Message, want)
	}

	if err := deleteNow(c, webhook); err != nil {
		t.Fatal(err)
	}
	reconcileProbe(t, r)

	assertUninstalled(t, c, "cdi", objects)
}

// Without its CR (the operator never deployed it, or it is gone) a component
// goes at once: there is nothing for its operator to remove.
func TestReconcile_WithoutTheOperatorCRTheUninstallGoesAtOnce(t *testing.T) {
	for _, component := range []string{"kubevirt", "cdi"} {
		t.Run(component, func(t *testing.T) {
			c, r, objects := virtOnEnvtest(t, component)

			reconcileProbe(t, r)

			assertUninstalled(t, c, component, objects)
		})
	}
}

// management Removed takes the same way: the component is Uninstalling, not
// Removed, until virt-operator is done.
func TestReconcile_RemovedKubeVirtGoesBeforeVirtOperatorToo(t *testing.T) {
	spec := k4allv1alpha1.ComponentSpec{
		Type: k4allv1alpha1.ComponentTypeManifest, Version: "v1", InlineManifests: []string{guardedConfigMap},
		Management: k4allv1alpha1.ComponentManagementRemoved,
	}
	c, r := installedOnEnvtestAs(t, "kubevirt", spec)
	objects := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "guarded", Namespace: "default"}}
	createAll(t, c, objects)
	runningOperatorCR(t, c, kubeVirtCR, kubeVirtFinalizer)

	reconcileProbe(t, r)

	assertOperatorKept(t, c, "kubevirt", objects)
	if st := componentStatus(t, c, "kubevirt"); st.State != k4allv1alpha1.ComponentStateUninstalling {
		t.Fatalf("kubevirt: %s (%s), want Uninstalling", st.State, st.Message)
	}

	kubeVirtCR.operatorDone(t, c)
	reconcileProbe(t, r)

	if err := c.Get(context.Background(), client.ObjectKeyFromObject(objects), &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Fatalf("kubevirt's objects: get = %v, want them removed", err)
	}
	if st := componentStatus(t, c, "kubevirt"); st.State != k4allv1alpha1.ComponentStateRemoved {
		t.Fatalf("kubevirt: %s (%s), want Removed", st.State, st.Message)
	}
}

// installedOnEnvtestAs is installedOnEnvtest for the component name.
func installedOnEnvtestAs(t *testing.T, name string, spec k4allv1alpha1.ComponentSpec) (client.Client, *ReleaseReconciler) {
	t.Helper()
	return componentOnEnvtest(t, name, spec, k4allv1alpha1.ComponentStatus{
		State: k4allv1alpha1.ComponentStateInstalled, InstalledVersion: "v1", LastTransitionTime: metav1.Now(),
	})
}

// k4all applies the KubeVirt and CDI CRs with spec.uninstallStrategy
// BlockUninstallIfWorkloadsExist (upstream's CRs leave it unset): a CR
// deleted by hand, or by anything else, then waits for the VMs and
// DataVolumes to be gone instead of deleting them. A value set by hand is
// replaced at the next apply.
func TestReconcile_KubeVirtAndCDIAreAppliedWithBlockUninstallIfWorkloadsExist(t *testing.T) {
	srv := upstreamReleases(t)
	for _, tc := range []struct {
		name, source, namespace string
		cr                      gatedCR
	}{
		{"KubeVirt", "/kubevirt/{{ version }}/kubevirt-cr.yaml", "kubevirt", kubeVirtCR},
		{"CDI", "/cdi/{{ version }}/cdi-cr.yaml", "cdi", cdiCR},
	} {
		t.Run(tc.name, func(t *testing.T) {
			version := "v1.9.1"
			if tc.name == "CDI" {
				version = "v1.66.1"
			}
			spec := k4allv1alpha1.ComponentSpec{
				Type: k4allv1alpha1.ComponentTypeManifest, Version: version,
				Sources: []string{srv + tc.source}, Namespace: tc.namespace,
			}
			c, r := onEnvtest(t, spec, k4allv1alpha1.ComponentStatus{})
			ensureNamespace(t, c, "kubevirt")
			tc.cr.deleteOnCleanup(t, c)
			strategy := func() string {
				s, _, _ := unstructured.NestedString(tc.cr.get(t, c).Object, "spec", "uninstallStrategy")
				return s
			}

			reconcileProbeTo(t, c, r, version, k4allv1alpha1.ComponentStateInstalled)
			if s := strategy(); s != blockUninstall {
				t.Fatalf("fresh install: uninstallStrategy %q, want BlockUninstallIfWorkloadsExist", s)
			}

			tc.cr.patchByHand(t, c, `{"spec":{"uninstallStrategy":"RemoveWorkloads"}}`)
			setProbe(t, c, k4allv1alpha1.ComponentSpec{
				Type: k4allv1alpha1.ComponentTypeManifest, Version: version,
				Sources: []string{srv + tc.source}, Namespace: tc.namespace, Custom: map[string]string{"apply": "again"},
			})
			reconcileProbeTo(t, c, r, version, k4allv1alpha1.ComponentStateInstalled)
			if s := strategy(); s != blockUninstall {
				t.Fatalf("next apply: uninstallStrategy %q, want BlockUninstallIfWorkloadsExist", s)
			}
		})
	}
}

// The release is not Ready while a component is half-removed: its operator
// still uninstalling what it created, stuck at it, or its uninstall refused.
// It was Ready ("10/10 components installed") with KubeVirt half-deleted.
func TestSetReadySummary_NotReadyWhileAComponentIsHalfRemoved(t *testing.T) {
	installed := k4allv1alpha1.ComponentStatus{State: k4allv1alpha1.ComponentStateInstalled, InstalledVersion: "v1"}
	for _, tc := range []struct {
		name         string
		kubevirt     k4allv1alpha1.ComponentStatus
		allOK        bool
		status       metav1.ConditionStatus
		reason, text string
	}{
		{"its operator uninstalls it", k4allv1alpha1.ComponentStatus{
			State: k4allv1alpha1.ComponentStateUninstalling, Message: "waiting for virt-operator to remove KubeVirt kubevirt/kubevirt",
		}, true, metav1.ConditionFalse, "ComponentsUninstalling", "components being uninstalled (see their status): kubevirt"},
		{"its uninstall is stuck", k4allv1alpha1.ComponentStatus{
			State: k4allv1alpha1.ComponentStateFailed, Message: "uninstall stuck: waiting for virt-operator …",
		}, false, metav1.ConditionFalse, "ComponentsFailed", "one or more components failed to reconcile"},
		{"its uninstall is refused", k4allv1alpha1.ComponentStatus{
			State: k4allv1alpha1.ComponentStateInstalled, Message: "uninstall refused: 1 VirtualMachine (default/vm1) exists: …",
		}, false, metav1.ConditionFalse, "UninstallRefused",
			"components kept installed, their uninstall refused (see their status): kubevirt"},
		{"it is gone", k4allv1alpha1.ComponentStatus{
			State: k4allv1alpha1.ComponentStateSkipped,
		}, true, metav1.ConditionTrue, "AllComponentsReconciled", "1/1 components installed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rm := &k4allv1alpha1.ReleaseManifest{Status: k4allv1alpha1.ReleaseManifestStatus{
				Components: map[string]k4allv1alpha1.ComponentStatus{"calico": installed, "kubevirt": tc.kubevirt},
			}}

			setReadySummary(rm, tc.allOK, 1)

			ready := meta.FindStatusCondition(rm.Status.Conditions, "Ready")
			if ready == nil || ready.Status != tc.status || ready.Reason != tc.reason || ready.Message != tc.text {
				t.Fatalf("Ready = %+v, want %s %s %q", ready, tc.status, tc.reason, tc.text)
			}
		})
	}
}

// Virt turned on again while virt-operator uninstalls KubeVirt: the install
// waits until the uninstall is over. Applied to the CR being deleted, it
// would be Installed once virt-operator is done, with nothing running.
func TestReconcile_KubeVirtTurnedBackOnWaitsForItsUninstall(t *testing.T) {
	spec := k4allv1alpha1.ComponentSpec{
		Type: k4allv1alpha1.ComponentTypeManifest, Version: "v1", InlineManifests: []string{guardedConfigMap},
		FeatureGate: "virt",
	}
	c, r := componentOnEnvtest(t, "kubevirt", spec, k4allv1alpha1.ComponentStatus{
		State: k4allv1alpha1.ComponentStateUninstalling, InstalledVersion: "v1",
		Message: "waiting for virt-operator to remove KubeVirt kubevirt/kubevirt: deleted", LastTransitionTime: metav1.Now(),
	})
	runningOperatorCR(t, c, kubeVirtCR, kubeVirtFinalizer)
	if err := c.Delete(context.Background(), kubeVirtCR.get(t, c)); err != nil {
		t.Fatal(err)
	}
	cc := &k4allv1alpha1.ClusterConfig{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "k4all-cluster-config"}, cc); err != nil {
		t.Fatal(err)
	}
	cc.Spec.Features.Virt.Enabled = true
	if err := c.Update(context.Background(), cc); err != nil {
		t.Fatal(err)
	}
	objects := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "guarded", Namespace: "default"}}
	t.Cleanup(func() { _ = c.Delete(context.Background(), objects) })

	reconcileProbe(t, r)

	if st := componentStatus(t, c, "kubevirt"); st.State != k4allv1alpha1.ComponentStateUninstalling {
		t.Fatalf("kubevirt: %s (%s), want Uninstalling until virt-operator is done", st.State, st.Message)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(objects), &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Fatalf("kubevirt's objects: get = %v, want them not applied yet", err)
	}

	kubeVirtCR.operatorDone(t, c)
	reconcileProbe(t, r)

	if st := componentStatus(t, c, "kubevirt"); st.State != k4allv1alpha1.ComponentStateInstalled {
		t.Fatalf("kubevirt: %s (%s), want Installed again", st.State, st.Message)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(objects), &corev1.ConfigMap{}); err != nil {
		t.Fatalf("kubevirt's objects: %v, want them applied", err)
	}
}

// previousOperatorHook is a component's hook as the previous operator had
// it: its install hooks only, no values revision.
type previousOperatorHook struct{ hooks.Hook }

// The operator that brings uninstallStrategy BlockUninstallIfWorkloadsExist
// re-applies KubeVirt and CDI once on every cluster with virt on, so their
// CRs get it at the operator upgrade, not at some later apply. The apply
// keeps what the cluster's admins set (feature gates, host devices), and
// the next reconcile applies nothing.
func TestReconcile_KubeVirtAndCDIAreAppliedOnceAfterTheOperatorUpgrade(t *testing.T) {
	srv := upstreamReleases(t)
	for _, tc := range []struct {
		name, version, source, namespace, byHand string
		cr                                       gatedCR
		gates                                    []string
	}{
		{"kubevirt", "v1.9.1", "/kubevirt/{{ version }}/kubevirt-cr.yaml", "kubevirt",
			`{"spec":{"uninstallStrategy":null,"configuration":{
				"developerConfiguration":{"featureGates":["HostDevices","Snapshot"]},
				"permittedHostDevices":{"pciHostDevices":[{"pciVendorSelector":"8086:46a6","resourceName":"example.com/iris-xe-vf"}]}}}}`,
			kubeVirtCR, []string{"HostDevices", "Snapshot"}},
		{"cdi", "v1.66.1", "/cdi/{{ version }}/cdi-cr.yaml", "cdi",
			`{"spec":{"uninstallStrategy":null,"config":{"featureGates":["HonorWaitForFirstConsumer","DataVolumeClaimAdoption"]}}}`,
			cdiCR, []string{"HonorWaitForFirstConsumer", "DataVolumeClaimAdoption"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := k4allv1alpha1.ComponentSpec{
				Type: k4allv1alpha1.ComponentTypeManifest, Version: tc.version,
				Sources: []string{srv + tc.source}, Namespace: tc.namespace,
			}
			c, previous := componentOnEnvtest(t, tc.name, spec, k4allv1alpha1.ComponentStatus{})
			ctx := context.Background()
			ensureNamespace(t, c, "kubevirt")
			tc.cr.deleteOnCleanup(t, c)
			cc := &k4allv1alpha1.ClusterConfig{}
			if err := c.Get(ctx, client.ObjectKey{Name: "k4all-cluster-config"}, cc); err != nil {
				t.Fatal(err)
			}
			cc.Spec.Features.Virt.Enabled = true
			if err := c.Update(ctx, cc); err != nil {
				t.Fatal(err)
			}

			// The previous operator installed it; the admins changed it since,
			// and its CR has no uninstallStrategy.
			previous.hookReg = hooks.NewRegistry(c, logr.Discard())
			previous.hookReg.Register(tc.name, previousOperatorHook{previous.hookReg.Get(tc.name)})
			reconcileProbe(t, previous)
			if st := componentStatus(t, c, tc.name); st.State != k4allv1alpha1.ComponentStateInstalled {
				t.Fatalf("previous operator: %s is %s (%s), want Installed", tc.name, st.State, st.Message)
			}
			tc.cr.patchByHand(t, c, tc.byHand)

			watching, err := client.NewWithWatch(envtestConfig(t), client.Options{Scheme: gateScheme(t)})
			if err != nil {
				t.Fatal(err)
			}
			applies := 0
			upgraded := &ReleaseReconciler{Scheme: previous.Scheme, RestConfig: previous.RestConfig, Log: logr.Discard(),
				Client: interceptor.NewClient(watching, interceptor.Funcs{
					Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
						if patch.Type() == types.ApplyPatchType && obj.GetObjectKind().GroupVersionKind().Kind == tc.cr.gvk.Kind {
							applies++
						}
						return cl.Patch(ctx, obj, patch, opts...)
					},
				})}

			reconcileProbe(t, upgraded)

			if applies != 1 {
				t.Fatalf("%s applied %d times at the first reconcile of the new operator, want once", tc.cr.gvk.Kind, applies)
			}
			cr := tc.cr.get(t, c)
			if strategy, _, _ := unstructured.NestedString(cr.Object, "spec", "uninstallStrategy"); strategy != blockUninstall {
				t.Errorf("uninstallStrategy %q, want %s", strategy, blockUninstall)
			}
			if gates := tc.cr.featureGates(t, c); !slices.Equal(gates, tc.gates) {
				t.Errorf("feature gates %v, want the ones set by hand %v", gates, tc.gates)
			}
			if tc.name == "kubevirt" {
				devices, _, _ := unstructured.NestedSlice(cr.Object, "spec", "configuration", "permittedHostDevices", "pciHostDevices")
				if len(devices) != 1 {
					t.Errorf("pciHostDevices %v, want the one set by hand", devices)
				}
			}

			reconcileProbe(t, upgraded)

			if applies != 1 {
				t.Fatalf("%s applied again at the next reconcile (%d applies in all), want once", tc.cr.gvk.Kind, applies)
			}
		})
	}
}
