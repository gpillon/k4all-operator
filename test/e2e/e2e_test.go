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

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "K4All Operator E2E Suite")
}

const (
	operatorNamespace   = "k4all-operator-system"
	deploymentName      = "k4all-operator-controller-manager"
	releaseManifestName = "k4all"
	clusterConfigName   = "k4all-cluster-config"

	testTimeout  = 15 * time.Minute
	pollInterval = 10 * time.Second
)

var root string

func init() {
	root = discoverProjectRoot()
}

// discoverProjectRoot walks upward from the test working directory until it
// finds go.mod, so that all kubectl paths are relative to the project root.
func discoverProjectRoot() string {
	cwd, err := os.Getwd()
	if err != nil {
		return "."
	}
	dir := cwd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return cwd
		}
		dir = parent
	}
}

func kubectl(args ...string) (string, error) {
	cmd := exec.Command("kubectl", args...)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func kubectlMustSucceed(args ...string) string {
	out, err := kubectl(args...)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "kubectl %v failed: %s", args, out)
	return out
}

func componentState(name string) string {
	out, _ := kubectl("get", "releasemanifest", releaseManifestName,
		"-o", fmt.Sprintf("jsonpath={.status.components.%s.state}", name))
	return out
}

func componentMessage(name string) string {
	out, _ := kubectl("get", "releasemanifest", releaseManifestName,
		"-o", fmt.Sprintf("jsonpath={.status.components.%s.message}", name))
	return out
}

func deploymentAvailable(name, namespace string) bool {
	out, err := kubectl("get", "deployment", name, "-n", namespace,
		"-o", "jsonpath={.status.conditions[?(@.type=='Available')].status}")
	return err == nil && out == "True"
}

func deploymentReady(name, namespace string) bool {
	out, err := kubectl("get", "deployment", name, "-n", namespace,
		"-o", "jsonpath={.status.readyReplicas}")
	return err == nil && out != "" && out != "0"
}

// expectedComponents lists components that the operator should install in
// the Kind test cluster (matching testdata/releasemanifest.yaml).
var expectedComponents = []string{
	"calico",
	"cert-manager",
	"metrics-server",
}

var _ = Describe("K4All Operator", Ordered, func() {

	BeforeAll(func() {
		By("verifying the cluster is reachable")
		Eventually(func() error {
			_, err := kubectl("cluster-info")
			return err
		}, 2*time.Minute, 5*time.Second).Should(Succeed())
	})

	// ── Operator Deployment ─────────────────────────────────────────────

	Context("Operator Deployment", func() {
		It("should deploy the operator from dist/install.yaml", func() {
			out, err := kubectl("apply", "-f", "dist/install.yaml")
			Expect(err).NotTo(HaveOccurred(), "apply install.yaml: %s", out)
		})

		It("should have the operator deployment available", func() {
			Eventually(func() bool {
				return deploymentAvailable(deploymentName, operatorNamespace)
			}, 3*time.Minute, pollInterval).Should(BeTrue(),
				"operator deployment not available")
		})

		It("should have the operator pod running", func() {
			Eventually(func() string {
				out, _ := kubectl("get", "pods", "-n", operatorNamespace,
					"-l", "control-plane=controller-manager",
					"-o", "jsonpath={.items[0].status.phase}")
				return out
			}, 2*time.Minute, pollInterval).Should(Equal("Running"))
		})
	})

	// ── CRD Registration ────────────────────────────────────────────────

	Context("CRD Registration", func() {
		crds := []string{
			"releasemanifests.k4all.magesgate.com",
			"clusterconfigs.k4all.magesgate.com",
			"nodeconfigs.k4all.magesgate.com",
		}
		for _, crd := range crds {
			crd := crd
			It(fmt.Sprintf("should have CRD %s", crd), func() {
				out := kubectlMustSucceed("get", "crd", crd)
				Expect(out).To(ContainSubstring(crd))
			})
		}
	})

	// ── Custom Resource Creation ────────────────────────────────────────

	Context("Custom Resources", func() {
		It("should accept a ClusterConfig CR", func() {
			out, err := kubectl("apply", "-f", "test/e2e/testdata/clusterconfig.yaml")
			Expect(err).NotTo(HaveOccurred(), "apply ClusterConfig: %s", out)
		})

		It("should accept a ReleaseManifest CR", func() {
			out, err := kubectl("apply", "-f", "test/e2e/testdata/releasemanifest.yaml")
			Expect(err).NotTo(HaveOccurred(), "apply ReleaseManifest: %s", out)
		})
	})

	// ── Reconciliation ──────────────────────────────────────────────────

	Context("Release Reconciliation", func() {
		It("should reconcile the ReleaseManifest to Ready", func() {
			Eventually(func() string {
				out, _ := kubectl("get", "releasemanifest", releaseManifestName,
					"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
				return out
			}, testTimeout, pollInterval).Should(Equal("True"),
				"ReleaseManifest never reached Ready=True")
		})

		It("should report all components installed", func() {
			out, err := kubectl("get", "releasemanifest", releaseManifestName,
				"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].message}")
			Expect(err).NotTo(HaveOccurred())
			expected := fmt.Sprintf("%d/%d components installed",
				len(expectedComponents), len(expectedComponents))
			Expect(out).To(ContainSubstring(expected),
				"Ready message should report %s, got: %s", expected, out)
		})
	})

	// ── Per-Component State ─────────────────────────────────────────────

	Context("Component States", func() {
		for _, comp := range expectedComponents {
			comp := comp
			It(fmt.Sprintf("should have %q in Installed state", comp), func() {
				state := componentState(comp)
				Expect(state).To(Equal("Installed"),
					"component %s state=%s message=%s", comp, state, componentMessage(comp))
			})
		}
	})

	// ── Workload Verification ───────────────────────────────────────────

	Context("Workload Verification", func() {
		It("should have tigera-operator running (calico)", func() {
			Eventually(func() bool {
				return deploymentReady("tigera-operator", "tigera-operator")
			}, 5*time.Minute, pollInterval).Should(BeTrue(),
				"tigera-operator deployment not ready")
		})

		It("should have cert-manager deployments running", func() {
			for _, dep := range []string{
				"cert-manager",
				"cert-manager-webhook",
				"cert-manager-cainjector",
			} {
				Eventually(func() bool {
					return deploymentReady(dep, "cert-manager")
				}, 5*time.Minute, pollInterval).Should(BeTrue(),
					"cert-manager deployment %s not ready", dep)
			}
		})

		It("should have metrics-server running", func() {
			Eventually(func() bool {
				return deploymentReady("metrics-server", "kube-system")
			}, 5*time.Minute, pollInterval).Should(BeTrue(),
				"metrics-server deployment not ready")
		})

		It("should have metrics-server patched with --kubelet-insecure-tls", func() {
			out, err := kubectl("get", "deployment", "metrics-server",
				"-n", "kube-system",
				"-o", "jsonpath={.spec.template.spec.containers[0].args}")
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring("--kubelet-insecure-tls"),
				"metrics-server missing patched arg, got: %s", out)
		})
	})

	// ── ClusterConfig Status ────────────────────────────────────────────

	Context("ClusterConfig Status", func() {
		It("should report active CNI as calico", func() {
			Eventually(func() string {
				out, _ := kubectl("get", "clusterconfig", clusterConfigName,
					"-o", "jsonpath={.status.activeCNI}")
				return out
			}, 2*time.Minute, pollInterval).Should(Equal("calico"))
		})

		It("should have Reconciled condition True", func() {
			Eventually(func() string {
				out, _ := kubectl("get", "clusterconfig", clusterConfigName,
					"-o", "jsonpath={.status.conditions[?(@.type=='Reconciled')].status}")
				return out
			}, 2*time.Minute, pollInterval).Should(Equal("True"))
		})

		It("should have a lastReconcileTime set", func() {
			out, err := kubectl("get", "clusterconfig", clusterConfigName,
				"-o", "jsonpath={.status.lastReconcileTime}")
			Expect(err).NotTo(HaveOccurred())
			Expect(out).NotTo(BeEmpty(), "lastReconcileTime should be set")
		})
	})

	// ── NodeConfig ──────────────────────────────────────────────────────

	Context("NodeConfig Creation", func() {
		It("should create a NodeConfig for each node", func() {
			Eventually(func() bool {
				nodeOut, err := kubectl("get", "nodes",
					"-o", "jsonpath={.items[*].metadata.name}")
				if err != nil || nodeOut == "" {
					return false
				}
				ncOut, err := kubectl("get", "nodeconfigs",
					"-o", "jsonpath={.items[*].metadata.name}")
				if err != nil {
					return false
				}
				nodes := strings.Fields(nodeOut)
				ncs := strings.Fields(ncOut)
				return len(ncs) >= len(nodes) && len(nodes) > 0
			}, 3*time.Minute, pollInterval).Should(BeTrue(),
				"NodeConfigs not created for all nodes")
		})

		It("should set nodeName in NodeConfig status", func() {
			out, err := kubectl("get", "nodeconfigs",
				"-o", "jsonpath={.items[0].status.nodeName}")
			Expect(err).NotTo(HaveOccurred())
			Expect(out).NotTo(BeEmpty(), "NodeConfig status.nodeName should be set")
		})
	})

	// ── Force Resync ────────────────────────────────────────────────────

	Context("Force Resync", func() {
		It("should process force-resync annotation and return to Ready", func() {
			By("annotating the ReleaseManifest with force-resync=all")
			_, err := kubectl("annotate", "releasemanifest", releaseManifestName,
				"k4all.magesgate.com/force-resync=all", "--overwrite")
			Expect(err).NotTo(HaveOccurred())

			By("waiting for the annotation to be consumed (removed)")
			Eventually(func() string {
				out, _ := kubectl("get", "releasemanifest", releaseManifestName,
					"-o", `jsonpath={.metadata.annotations.k4all\.magesgate\.com/force-resync}`)
				return out
			}, 3*time.Minute, pollInterval).Should(BeEmpty())

			By("waiting for ReleaseManifest to return to Ready")
			Eventually(func() string {
				out, _ := kubectl("get", "releasemanifest", releaseManifestName,
					"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
				return out
			}, testTimeout, pollInterval).Should(Equal("True"))
		})
	})

	// ── Single Component Resync ─────────────────────────────────────────

	Context("Single Component Resync", func() {
		It("should resync only the targeted component", func() {
			By("annotating with force-resync=cert-manager")
			_, err := kubectl("annotate", "releasemanifest", releaseManifestName,
				"k4all.magesgate.com/force-resync=cert-manager", "--overwrite")
			Expect(err).NotTo(HaveOccurred())

			By("waiting for the annotation to be consumed")
			Eventually(func() string {
				out, _ := kubectl("get", "releasemanifest", releaseManifestName,
					"-o", `jsonpath={.metadata.annotations.k4all\.magesgate\.com/force-resync}`)
				return out
			}, 3*time.Minute, pollInterval).Should(BeEmpty())

			By("waiting for cert-manager to return to Installed")
			Eventually(func() string {
				return componentState("cert-manager")
			}, testTimeout, pollInterval).Should(Equal("Installed"))

			By("verifying ReleaseManifest returns to Ready")
			Eventually(func() string {
				out, _ := kubectl("get", "releasemanifest", releaseManifestName,
					"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
				return out
			}, testTimeout, pollInterval).Should(Equal("True"))
		})
	})

	// ── Diagnostics ─────────────────────────────────────────────────────

	AfterAll(func() {
		By("capturing final ReleaseManifest status for diagnostics")
		out, _ := kubectl("get", "releasemanifest", releaseManifestName,
			"-o", "jsonpath={.status}")
		if out != "" {
			var status map[string]interface{}
			if err := json.Unmarshal([]byte(out), &status); err == nil {
				formatted, _ := json.MarshalIndent(status, "", "  ")
				fmt.Fprintf(GinkgoWriter, "\n=== ReleaseManifest Status ===\n%s\n", formatted)
			}
		}

		By("capturing operator logs for diagnostics")
		logs, _ := kubectl("logs", "-n", operatorNamespace,
			"-l", "control-plane=controller-manager",
			"--tail=200")
		fmt.Fprintf(GinkgoWriter, "\n=== Operator Logs (last 200 lines) ===\n%s\n", logs)

		By("capturing pod status across namespaces")
		for _, ns := range []string{operatorNamespace, "tigera-operator", "cert-manager", "kube-system"} {
			pods, _ := kubectl("get", "pods", "-n", ns, "-o", "wide")
			fmt.Fprintf(GinkgoWriter, "\n=== Pods in %s ===\n%s\n", ns, pods)
		}
	})
})
