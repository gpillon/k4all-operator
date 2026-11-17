/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package e2eupgrade runs a ClusterUpgrade on a multi-node kind cluster
// (kind.yaml). A fake k4all-upgrade in the test process plays the nodes'
// side of the protocol through the same Node annotations.
package e2eupgrade

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

func TestE2EUpgrade(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "K4All ClusterUpgrade E2E Suite")
}

const (
	operatorNamespace = "k4all-operator-system"
	operatorName      = "k4all-operator-controller-manager"
	testNamespace     = "k4all-e2e-upgrade"
	graphURL          = "http://graph.k4all-e2e-upgrade.svc.cluster.local/upgrade-graph.json"
	startRelease      = "v2.5.0"
	targetRelease     = "v2.6.1"
	pollInterval      = 5 * time.Second

	// node-type values the fake agent reports.
	roleBootstrap = "bootstrap"
	roleControl   = "control"
	roleWorker    = "worker"
)

var root = projectRoot()

func projectRoot() string {
	dir, _ := os.Getwd()
	for d := dir; d != filepath.Dir(d); d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
	}
	return dir
}

func kubectl(args ...string) (string, error) {
	cmd := exec.Command("kubectl", args...)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func kubectlMustSucceed(args ...string) string {
	out, err := kubectl(args...)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "kubectl %v: %s", args, out)
	return out
}

func getJSON(into interface{}, args ...string) error {
	out, err := kubectl(append(args, "-o", "json")...)
	if err != nil {
		return fmt.Errorf("kubectl %v: %s", args, out)
	}
	return json.Unmarshal([]byte(out), into)
}

func clusterUpgrade() (*k4allv1alpha1.ClusterUpgrade, error) {
	cu := &k4allv1alpha1.ClusterUpgrade{}
	return cu, getJSON(cu, "get", "clusterupgrade", k4allv1alpha1.ClusterUpgradeName)
}

// ── Fake k4all-upgrade ──────────────────────────────────────────────────

// hopRecord is what the fake agent saw when it started a node's hop.
type hopRecord struct {
	node, release string
	cordoned      bool
	podsLeft      []string // pods the drain should have moved
	otherTargets  []string // other nodes asked for a hop at the same time
}

// fakeAgent answers upgrade-target like k4all-upgrade (contract §5.4): it
// writes release and node-type, runs a hop in a few seconds through the
// phases STAGE and REBOOT under upgrade-in-progress, then commits the release
// and writes upgrade-done.
type fakeAgent struct {
	roles map[string]string // node -> node-type it reports
	stop  chan struct{}
	done  chan struct{}

	mu      sync.Mutex
	hops    []hopRecord
	started map[string]time.Time // node -> start of its running hop
	errs    []string
}

func (a *fakeAgent) run() {
	defer close(a.done)
	for {
		select {
		case <-a.stop:
			return
		case <-time.After(2 * time.Second):
		}
		if err := a.pass(); err != nil {
			a.mu.Lock()
			a.errs = append(a.errs, err.Error())
			a.mu.Unlock()
		}
	}
}

func annotate(node string, kv ...string) error {
	args := append([]string{"annotate", "node", node, "--overwrite"}, kv...)
	if out, err := kubectl(args...); err != nil {
		return fmt.Errorf("annotate %s: %s", node, out)
	}
	return nil
}

func (a *fakeAgent) pass() error {
	nodes := &corev1.NodeList{}
	if err := getJSON(nodes, "get", "nodes"); err != nil {
		return err
	}
	for _, n := range nodes.Items {
		ann := n.Annotations
		if ann[k4allv1alpha1.NodeReleaseAnnotation] == "" {
			if err := annotate(n.Name, k4allv1alpha1.NodeReleaseAnnotation+"="+startRelease,
				k4allv1alpha1.NodeTypeAnnotation+"="+a.roles[n.Name]); err != nil {
				return err
			}
			continue
		}
		target, release := ann[k4allv1alpha1.NodeUpgradeTargetAnnotation], ann[k4allv1alpha1.NodeReleaseAnnotation]
		a.mu.Lock()
		started, running := a.started[n.Name]
		a.mu.Unlock()
		switch {
		case target == "" || ann[k4allv1alpha1.NodeUpgradeDoneAnnotation] == target:
		case target == release && !running:
			if err := annotate(n.Name, k4allv1alpha1.NodeUpgradeDoneAnnotation+"="+target); err != nil {
				return err
			}
		case !running:
			a.record(n, target, nodes)
			if err := annotate(n.Name, "k4all.magesgate.com/upgrade-in-progress="+release+"->"+target,
				k4allv1alpha1.NodeUpgradePhaseAnnotation+"=STAGE"); err != nil {
				return err
			}
		case time.Since(started) < 4*time.Second:
			if err := annotate(n.Name, k4allv1alpha1.NodeUpgradePhaseAnnotation+"=REBOOT"); err != nil {
				return err
			}
		default: // COMMIT
			if err := annotate(n.Name, k4allv1alpha1.NodeReleaseAnnotation+"="+target,
				"k4all.magesgate.com/upgrade-in-progress-", k4allv1alpha1.NodeUpgradePhaseAnnotation+"-"); err != nil {
				return err
			}
			if err := annotate(n.Name, k4allv1alpha1.NodeUpgradeDoneAnnotation+"="+target); err != nil {
				return err
			}
			a.mu.Lock()
			delete(a.started, n.Name)
			a.mu.Unlock()
		}
	}
	return nil
}

// record notes the state of the cluster as node n starts its hop to target.
func (a *fakeAgent) record(n corev1.Node, target string, nodes *corev1.NodeList) {
	rec := hopRecord{node: n.Name, release: target, cordoned: n.Spec.Unschedulable}
	for _, other := range nodes.Items {
		if other.Name != n.Name && other.Annotations[k4allv1alpha1.NodeUpgradeTargetAnnotation] != "" &&
			other.Annotations[k4allv1alpha1.NodeUpgradeDoneAnnotation] == "" {
			rec.otherTargets = append(rec.otherTargets, other.Name)
		}
	}
	pods := &corev1.PodList{}
	if err := getJSON(pods, "get", "pods", "-A", "--field-selector", "spec.nodeName="+n.Name); err == nil {
		for _, p := range pods.Items {
			if movable(p) {
				rec.podsLeft = append(rec.podsLeft, p.Namespace+"/"+p.Name)
			}
		}
	}
	a.mu.Lock()
	a.hops = append(a.hops, rec)
	a.started[n.Name] = time.Now()
	a.mu.Unlock()
}

// movable is a pod the drain must have moved: not a DaemonSet or static pod,
// not finished, not the operator.
func movable(p corev1.Pod) bool {
	if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed ||
		p.Namespace == operatorNamespace {
		return false
	}
	if _, mirror := p.Annotations[corev1.MirrorPodAnnotationKey]; mirror {
		return false
	}
	for _, o := range p.OwnerReferences {
		if o.Kind == "DaemonSet" {
			return false
		}
	}
	return true
}

// ── Specs ───────────────────────────────────────────────────────────────

var _ = Describe("ClusterUpgrade", Ordered, func() {
	var agent *fakeAgent
	var order []string // nodes in upgrade order: bootstrap, control, worker

	BeforeAll(func() {
		By("deploying the operator from dist/install.yaml")
		kubectlMustSucceed("apply", "-f", "dist/install.yaml")
		kubectlMustSucceed("rollout", "status", "-n", operatorNamespace, "deployment/"+operatorName, "--timeout=5m")

		By("serving the upgrade graph and running a workload to drain")
		kubectlMustSucceed("apply", "-f", "test/e2e-upgrade/testdata/graph.yaml")
		kubectlMustSucceed("apply", "-f", "test/e2e-upgrade/testdata/workload.yaml")
		kubectlMustSucceed("rollout", "status", "-n", testNamespace, "deployment/graph", "--timeout=3m")
		kubectlMustSucceed("rollout", "status", "-n", testNamespace, "deployment/workload", "--timeout=3m")

		By("starting the fake k4all-upgrade on every node")
		nodes := &corev1.NodeList{}
		Expect(getJSON(nodes, "get", "nodes")).To(Succeed())
		roles := map[string]string{}
		var workers []string
		for _, n := range nodes.Items {
			if _, cp := n.Labels["node-role.kubernetes.io/control-plane"]; cp {
				roles[n.Name] = roleBootstrap
				order = append([]string{n.Name}, order...)
			} else {
				workers = append(workers, n.Name)
			}
		}
		Expect(order).To(HaveLen(1), "kind.yaml has one control plane")
		Expect(workers).To(HaveLen(2), "kind.yaml has two workers")
		// kind names them <cluster>-worker and <cluster>-worker2: the first
		// plays a control plane, to check the order of the three roles.
		if workers[0] > workers[1] {
			workers[0], workers[1] = workers[1], workers[0]
		}
		roles[workers[0]], roles[workers[1]] = roleControl, roleWorker
		order = append(order, workers...)
		agent = &fakeAgent{
			roles: roles, stop: make(chan struct{}), done: make(chan struct{}), started: map[string]time.Time{},
		}
		go agent.run()
	})

	AfterAll(func() {
		if agent != nil {
			close(agent.stop)
			<-agent.done
			for _, h := range agent.hops {
				_, _ = fmt.Fprintf(GinkgoWriter, "hop %s on %s: cordoned=%v podsLeft=%v others=%v\n",
					h.release, h.node, h.cordoned, h.podsLeft, h.otherTargets)
			}
			for _, e := range agent.errs {
				_, _ = fmt.Fprintf(GinkgoWriter, "fake agent: %s\n", e)
			}
		}
		for _, args := range [][]string{
			{"get", "clusterupgrade", k4allv1alpha1.ClusterUpgradeName, "-o", "yaml"},
			{"get", "nodes", "-o",
				"custom-columns=NAME:.metadata.name,UNSCHEDULABLE:.spec.unschedulable,ANNOTATIONS:.metadata.annotations"},
			{"get", "pods", "-A", "-o", "wide"},
			{"logs", "-n", operatorNamespace, "deployment/" + operatorName, "--tail=200"},
		} {
			out, _ := kubectl(args...)
			_, _ = fmt.Fprintf(GinkgoWriter, "\n=== kubectl %s ===\n%s\n", strings.Join(args, " "), out)
		}
	})

	It("should report the cluster's release and the releases it can move to", func() {
		By("pointing the ClusterUpgrade the operator created at the test graph")
		Eventually(func() error {
			_, err := kubectl("patch", "clusterupgrade", k4allv1alpha1.ClusterUpgradeName, "--type=merge",
				"-p", `{"spec":{"graphUrl":"`+graphURL+`"}}`)
			return err
		}, 2*time.Minute, pollInterval).Should(Succeed(), "the operator never created the ClusterUpgrade")

		Eventually(func(g Gomega) {
			cu, err := clusterUpgrade()
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(cu.Status.Phase).To(Equal(k4allv1alpha1.ClusterUpgradeIdle))
			g.Expect(cu.Status.CurrentRelease).To(Equal(startRelease))
			var available []string
			for _, r := range cu.Status.AvailableReleases {
				available = append(available, r.Version)
			}
			g.Expect(available).To(Equal([]string{"v2.6.0", targetRelease}))
			g.Expect(cu.Status.Nodes).To(HaveLen(3))
		}, 3*time.Minute, pollInterval).Should(Succeed())
	})

	It("should upgrade one drained node at a time, bootstrap first, every node a hop before the next", func() {
		kubectlMustSucceed("patch", "clusterupgrade", k4allv1alpha1.ClusterUpgradeName, "--type=merge",
			"-p", `{"spec":{"targetRelease":"`+targetRelease+`"}}`)

		Eventually(func(g Gomega) {
			cu, err := clusterUpgrade()
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(cu.Status.Phase).To(Equal(k4allv1alpha1.ClusterUpgradeSucceeded),
				"%s: %s", cu.Status.Phase, cu.Status.Message)
		}, 20*time.Minute, pollInterval).Should(Succeed())

		By("checking the order of the hops")
		agent.mu.Lock()
		hops := append([]hopRecord(nil), agent.hops...)
		agent.mu.Unlock()
		var got, want []string
		for _, h := range hops {
			got = append(got, h.release+" "+h.node)
		}
		for _, release := range []string{"v2.6.0", targetRelease} {
			for _, n := range order {
				want = append(want, release+" "+n)
			}
		}
		Expect(got).To(Equal(want))

		By("checking each node was cordoned and drained, alone, when its hop started")
		for _, h := range hops {
			Expect(h.cordoned).To(BeTrue(), "%s started %s uncordoned", h.node, h.release)
			Expect(h.podsLeft).To(BeEmpty(), "%s started %s with pods to move", h.node, h.release)
			Expect(h.otherTargets).To(BeEmpty(), "%s started %s while other nodes had a hop", h.node, h.release)
		}

		By("checking the nodes are uncordoned and the protocol annotations cleared")
		nodes := &corev1.NodeList{}
		Expect(getJSON(nodes, "get", "nodes")).To(Succeed())
		for _, n := range nodes.Items {
			Expect(n.Spec.Unschedulable).To(BeFalse(), "%s left cordoned", n.Name)
			Expect(n.Annotations).To(HaveKeyWithValue(k4allv1alpha1.NodeReleaseAnnotation, targetRelease))
			for _, key := range []string{
				k4allv1alpha1.NodeUpgradeTargetAnnotation,
				k4allv1alpha1.NodeUpgradeDoneAnnotation,
				k4allv1alpha1.NodeUpgradeCordonedAnnotation,
			} {
				Expect(n.Annotations).NotTo(HaveKey(key), "%s keeps %s", n.Name, key)
			}
		}

		cu, err := clusterUpgrade()
		Expect(err).NotTo(HaveOccurred())
		Expect(cu.Status.Path).To(Equal([]k4allv1alpha1.Hop{
			{Release: "v2.6.0", Kind: "minor", Phase: k4allv1alpha1.HopDone},
			{Release: targetRelease, Kind: "patch", Phase: k4allv1alpha1.HopDone},
		}))
		kubectlMustSucceed("rollout", "status", "-n", testNamespace, "deployment/workload", "--timeout=3m")
	})
})
