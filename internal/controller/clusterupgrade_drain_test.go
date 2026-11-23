package controller

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/clusterupgrade"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

// drainingLabel is on a Node from its cordon to its uncordon: the operator's
// pod tolerates the cordon and prefers the nodes without it (k4all#86).
// k4all-upgrade sets the same label when it drains a node itself.
const drainingLabel, drainingOn = "k4all.magesgate.com/draining", "true"

var cnpgPrimaryLabels = map[string]string{"cnpg.io/cluster": "pg", "cnpg.io/instanceRole": "primary"}

func TestClusterUpgrade_LabelsTheNodeItDrainsUntilTheUncordon(t *testing.T) {
	b := newUpgradeTestbed(t,
		upgradeNode("cu-cp", "bootstrap", "v2.5.0"),
		upgradeNode("cu-w", "worker", "v2.5.0"))
	b.reconcile()
	b.targetNextRelease()

	b.reconcile() // cordon
	if n := b.node("cu-cp"); !n.Spec.Unschedulable || n.Labels[drainingLabel] != drainingOn {
		t.Fatalf("cu-cp unschedulable=%v labels=%v, want cordoned with %s=true", n.Spec.Unschedulable, n.Labels, drainingLabel)
	}
	if _, ok := b.node("cu-w").Labels[drainingLabel]; ok {
		t.Errorf("cu-w, not drained, has the %s label", drainingLabel)
	}

	b.reconcile() // nothing to evict: upgrade-target
	n := b.node("cu-cp")
	if n.Labels[drainingLabel] != drainingOn {
		t.Errorf("labels = %v during the hop, want %s=true until the uncordon", n.Labels, drainingLabel)
	}
	n.Annotations[k4allv1alpha1.NodeUpgradePhaseAnnotation] = "SYNC" // past the reboot
	if err := b.c.Update(context.Background(), n); err != nil {
		t.Fatal(err)
	}

	b.reconcile() // uncordon
	n = b.node("cu-cp")
	if n.Spec.Unschedulable {
		t.Fatalf("cu-cp still cordoned in SYNC")
	}
	if v, ok := n.Labels[drainingLabel]; ok {
		t.Errorf("%s=%q left after the uncordon", drainingLabel, v)
	}
}

// The label goes at the end of the hop also when the cordon was the admin's,
// which stays.
func TestClusterUpgrade_FinishRemovesTheLabelAndKeepsAnAdminCordon(t *testing.T) {
	cp := upgradeNode("cu-cp", "bootstrap", "v2.5.0")
	cp.Spec.Unschedulable = true
	b := newUpgradeTestbed(t, cp, upgradeNode("cu-w", "worker", "v2.5.0"))
	b.reconcile()
	b.targetNextRelease()

	b.reconcile() // cordon: already
	n := b.node("cu-cp")
	if n.Annotations[k4allv1alpha1.NodeUpgradeCordonedAnnotation] != k4allv1alpha1.NodeCordonedAlready || n.Labels[drainingLabel] != drainingOn {
		t.Fatalf("cu-cp cordoned=%q labels=%v, want already with %s=true",
			n.Annotations[k4allv1alpha1.NodeUpgradeCordonedAnnotation], n.Labels, drainingLabel)
	}
	b.reconcile() // upgrade-target
	n = b.node("cu-cp")
	n.Annotations[k4allv1alpha1.NodeReleaseAnnotation] = nextRelease
	n.Annotations[k4allv1alpha1.NodeUpgradeDoneAnnotation] = nextRelease
	if err := b.c.Update(context.Background(), n); err != nil {
		t.Fatal(err)
	}

	b.reconcile() // finish
	n = b.node("cu-cp")
	if _, ok := n.Annotations[k4allv1alpha1.NodeUpgradeCordonedAnnotation]; ok {
		t.Fatalf("cu-cp not finished: annotations %v", n.Annotations)
	}
	if !n.Spec.Unschedulable {
		t.Errorf("the operator uncordoned a node the admin had cordoned")
	}
	if v, ok := n.Labels[drainingLabel]; ok {
		t.Errorf("%s=%q left after the hop", drainingLabel, v)
	}
}

// runningPod makes p Running and Ready, as a kubelet would.
func (b *upgradeTestbed) runningPod(p *corev1.Pod, labels map[string]string) {
	b.t.Helper()
	p.Labels = labels
	if err := b.c.Update(context.Background(), p); err != nil {
		b.t.Fatal(err)
	}
	p.Status = corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{
		{Type: corev1.PodReady, Status: corev1.ConditionTrue},
	}}
	if err := b.c.Status().Update(context.Background(), p); err != nil {
		b.t.Fatal(err)
	}
}

// pdb creates a budget on the pods with labels, with the status the
// disruption controller (absent from envtest) would report.
func (b *upgradeTestbed) pdb(name string, labels map[string]string, status policyv1.PodDisruptionBudgetStatus) *policyv1.PodDisruptionBudget {
	b.t.Helper()
	minAvailable := intstr.FromInt32(1)
	p := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &minAvailable,
			Selector:     &metav1.LabelSelector{MatchLabels: labels},
		},
	}
	if err := b.c.Create(context.Background(), p); err != nil {
		b.t.Fatal(err)
	}
	b.t.Cleanup(func() { _ = b.c.Delete(context.Background(), p) })
	status.ObservedGeneration = p.Generation
	p.Status = status
	if err := b.c.Status().Update(context.Background(), p); err != nil {
		b.t.Fatal(err)
	}
	return p
}

// A single-instance CloudNativePG primary: its budget is healthy (1/1) and
// allows no disruption, so a drain of its node would never end (k4all#87).
// The operator does not cordon that node, and says why.
func TestClusterUpgrade_DoesNotCordonANodeItCouldNeverDrain(t *testing.T) {
	b := newUpgradeTestbed(t,
		upgradeNode("cu-cp", "bootstrap", "v2.5.0"),
		upgradeNode("cu-w", "worker", "v2.5.0"))
	b.reconcile()
	b.runningPod(b.pod("pg-1"), cnpgPrimaryLabels)
	pdb := b.pdb("pg-primary", cnpgPrimaryLabels, policyv1.PodDisruptionBudgetStatus{
		DisruptionsAllowed: 0, CurrentHealthy: 1, DesiredHealthy: 1, ExpectedPods: 1})
	b.targetNextRelease()

	cu := b.reconcile()
	n := b.node("cu-cp")
	if n.Spec.Unschedulable || n.Annotations[k4allv1alpha1.NodeUpgradeCordonedAnnotation] != "" || n.Labels[drainingLabel] != "" {
		t.Fatalf("cu-cp unschedulable=%v cordoned=%q labels=%v, want it left alone",
			n.Spec.Unschedulable, n.Annotations[k4allv1alpha1.NodeUpgradeCordonedAnnotation], n.Labels)
	}
	want := "cu-cp cannot be drained: PDB default/pg-primary allows no disruption of pod default/pg-1"
	if !strings.Contains(cu.Status.Message, want) {
		t.Errorf("message = %q, want %q in it", cu.Status.Message, want)
	}
	if cu.Status.Phase != k4allv1alpha1.ClusterUpgradeProgressing {
		t.Errorf("phase = %s, want Progressing (waiting, not Failed)", cu.Status.Phase)
	}

	// The admin lets the database go for the hop: CNPG deletes the budget.
	if err := b.c.Delete(context.Background(), pdb); err != nil {
		t.Fatal(err)
	}
	b.reconcile()
	if n := b.node("cu-cp"); !n.Spec.Unschedulable {
		t.Errorf("cu-cp not cordoned once its budget is gone")
	}
}

func testPod(name, node string, labels map[string]string, mutate ...func(*corev1.Pod)) corev1.Pod {
	p := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "db", Name: name, Labels: labels},
		Spec:       corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionTrue},
		}},
	}
	for _, m := range mutate {
		m(&p)
	}
	return p
}

// testPDB is the primary budget of the CloudNativePG cluster db/pg.
func testPDB(selector *metav1.LabelSelector, mutate ...func(*policyv1.PodDisruptionBudget)) policyv1.PodDisruptionBudget {
	p := policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Namespace: "db", Name: "pg-primary", Generation: 1},
		Spec:       policyv1.PodDisruptionBudgetSpec{Selector: selector},
		Status: policyv1.PodDisruptionBudgetStatus{
			ObservedGeneration: 1, DisruptionsAllowed: 0, CurrentHealthy: 1, DesiredHealthy: 1, ExpectedPods: 1},
	}
	for _, m := range mutate {
		m(&p)
	}
	return p
}

func TestFindBlockers(t *testing.T) {
	primarySelector := &metav1.LabelSelector{MatchLabels: cnpgPrimaryLabels}
	replicaLabels := map[string]string{"cnpg.io/cluster": "pg", "cnpg.io/instanceRole": "replica"}
	self := types.NamespacedName{Namespace: "db", Name: "operator"}
	blocked := map[string][]clusterupgrade.Blocker{"n1": {{PDB: "db/pg-primary", Pod: "db/pg-1"}}}
	none := map[string][]clusterupgrade.Blocker{}
	primary := testPod("pg-1", "n1", cnpgPrimaryLabels)

	for name, tc := range map[string]struct {
		pdbs []policyv1.PodDisruptionBudget
		pods []corev1.Pod
		want map[string][]clusterupgrade.Blocker
	}{
		"single-instance primary": {[]policyv1.PodDisruptionBudget{testPDB(primarySelector)}, []corev1.Pod{primary}, blocked},
		"budget allows one": {[]policyv1.PodDisruptionBudget{testPDB(primarySelector, func(p *policyv1.PodDisruptionBudget) {
			p.Status.DisruptionsAllowed, p.Status.CurrentHealthy, p.Status.ExpectedPods = 1, 2, 2
		})}, []corev1.Pod{primary}, none},
		"pods still starting": {[]policyv1.PodDisruptionBudget{testPDB(primarySelector, func(p *policyv1.PodDisruptionBudget) {
			p.Status.CurrentHealthy = 0
		})}, []corev1.Pod{primary}, none},
		"not observed yet": {[]policyv1.PodDisruptionBudget{testPDB(primarySelector, func(p *policyv1.PodDisruptionBudget) {
			p.Generation = 2
		})}, []corev1.Pod{primary}, none},
		"selector misses": {[]policyv1.PodDisruptionBudget{testPDB(&metav1.LabelSelector{MatchLabels: map[string]string{"app": "x"}})},
			[]corev1.Pod{primary}, none},
		"null selector selects nothing": {[]policyv1.PodDisruptionBudget{testPDB(nil)}, []corev1.Pod{primary}, none},
		"empty selector selects every pod": {[]policyv1.PodDisruptionBudget{testPDB(&metav1.LabelSelector{})},
			[]corev1.Pod{primary}, blocked},
		"other namespace": {[]policyv1.PodDisruptionBudget{testPDB(primarySelector)},
			[]corev1.Pod{testPod("pg-1", "n1", cnpgPrimaryLabels, func(p *corev1.Pod) { p.Namespace = "other" })}, none},
		"daemonset pod": {[]policyv1.PodDisruptionBudget{testPDB(primarySelector)},
			[]corev1.Pod{testPod("pg-1", "n1", cnpgPrimaryLabels, func(p *corev1.Pod) {
				p.OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Name: "ds"}}
			})}, none},
		"terminating pod": {[]policyv1.PodDisruptionBudget{testPDB(primarySelector)},
			[]corev1.Pod{testPod("pg-1", "n1", cnpgPrimaryLabels, func(p *corev1.Pod) { p.DeletionTimestamp = &metav1.Time{Time: time.Now()} })}, none},
		"pending pod": {[]policyv1.PodDisruptionBudget{testPDB(primarySelector)},
			[]corev1.Pod{testPod("pg-1", "n1", cnpgPrimaryLabels, func(p *corev1.Pod) { p.Status.Phase = corev1.PodPending })}, none},
		"mirror pod": {[]policyv1.PodDisruptionBudget{testPDB(primarySelector)},
			[]corev1.Pod{testPod("pg-1", "n1", cnpgPrimaryLabels, func(p *corev1.Pod) {
				p.Annotations = map[string]string{corev1.MirrorPodAnnotationKey: "x"}
			})}, none},
		"the operator itself": {[]policyv1.PodDisruptionBudget{testPDB(&metav1.LabelSelector{})},
			[]corev1.Pod{testPod("operator", "n1", nil)}, none},
		// CNPG switches the primary over when its node is cordoned.
		"cnpg primary with a ready replica elsewhere": {[]policyv1.PodDisruptionBudget{testPDB(primarySelector)},
			[]corev1.Pod{primary, testPod("pg-2", "n2", replicaLabels)}, none},
		"cnpg replica on the same node": {[]policyv1.PodDisruptionBudget{testPDB(primarySelector)},
			[]corev1.Pod{primary, testPod("pg-2", "n1", replicaLabels)}, blocked},
		"cnpg replica not ready": {[]policyv1.PodDisruptionBudget{testPDB(primarySelector)},
			[]corev1.Pod{primary, testPod("pg-2", "n2", replicaLabels, func(p *corev1.Pod) { p.Status.Conditions = nil })}, blocked},
	} {
		t.Run(name, func(t *testing.T) {
			got := findBlockers(tc.pdbs, tc.pods, self)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("findBlockers = %v, want %v", got, tc.want)
			}
		})
	}
}

// install.yaml is `kustomize build config/default`: the operator keeps
// tolerating the cordon (a single node must run it to uncordon itself) and
// prefers the nodes that are not being drained (k4all#86).
func TestInstallManifest_OperatorToleratesTheCordonAndAvoidsDrainingNodes(t *testing.T) {
	resources, err := krusty.MakeKustomizer(krusty.MakeDefaultOptions()).
		Run(filesys.MakeFsOnDisk(), filepath.Join("..", "..", "config", "default"))
	if err != nil {
		t.Fatalf("kustomize build config/default: %v", err)
	}
	var dep *appsv1.Deployment
	for _, res := range resources.Resources() {
		if res.GetKind() != "Deployment" {
			continue
		}
		data, err := res.MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		dep = &appsv1.Deployment{}
		if err := json.Unmarshal(data, dep); err != nil {
			t.Fatal(err)
		}
	}
	if dep == nil || dep.Name != "k4all-operator-controller-manager" {
		t.Fatalf("no k4all-operator-controller-manager Deployment in the build (got %v)", dep)
	}
	spec := dep.Spec.Template.Spec

	tolerates := false
	for _, tol := range spec.Tolerations {
		if tol.Key == corev1.TaintNodeUnschedulable && tol.Operator == corev1.TolerationOpExists && tol.Effect == corev1.TaintEffectNoSchedule {
			tolerates = true
		}
	}
	if !tolerates {
		t.Errorf("tolerations = %+v, want node.kubernetes.io/unschedulable Exists NoSchedule", spec.Tolerations)
	}

	if spec.Affinity == nil || spec.Affinity.NodeAffinity == nil {
		t.Fatalf("affinity = %+v, want a preferred node affinity away from %s", spec.Affinity, drainingLabel)
	}
	na := spec.Affinity.NodeAffinity
	if na.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		t.Errorf("required node affinity %+v: a single node must still fit", na.RequiredDuringSchedulingIgnoredDuringExecution)
	}
	want := corev1.PreferredSchedulingTerm{Weight: 100, Preference: corev1.NodeSelectorTerm{
		MatchExpressions: []corev1.NodeSelectorRequirement{
			{Key: drainingLabel, Operator: corev1.NodeSelectorOpNotIn, Values: []string{"true"}},
		},
	}}
	found := false
	for _, term := range na.PreferredDuringSchedulingIgnoredDuringExecution {
		if reflect.DeepEqual(term, want) {
			found = true
		}
	}
	if !found {
		t.Errorf("preferred node affinity = %+v, want %+v", na.PreferredDuringSchedulingIgnoredDuringExecution, want)
	}
}
