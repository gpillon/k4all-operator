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
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/yaml"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
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

	// The ingress controller of k4all v2.4.0 on, as ingressController names it.
	traefikController = "traefik"

	testTimeout  = 15 * time.Minute
	pollInterval = 10 * time.Second
)

var root string

// releaseManifestFile is the ReleaseManifest installed first; upgradeChain
// (E2E_UPGRADE_TO, comma-separated) lists the ones applied after it, in order,
// as successive k4all releases sync them on an existing cluster.
// clusterConfigFile is the ClusterConfig the cluster was installed with.
var (
	clusterConfigFile   = envOr("E2E_CLUSTER_CONFIG", "test/e2e/testdata/clusterconfig.yaml")
	releaseManifestFile = envOr("E2E_RELEASE_MANIFEST", "test/e2e/testdata/releasemanifest.yaml")
	upgradeChain        = splitList(os.Getenv("E2E_UPGRADE_TO"))
)

func splitList(v string) []string {
	var out []string
	for _, item := range strings.Split(v, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

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

// componentStatus returns "<state>/<installedVersion>" for a component.
func componentStatus(name string) string {
	out, _ := kubectl("get", "releasemanifest", releaseManifestName, "-o", fmt.Sprintf(
		"jsonpath={.status.components.%s.state}/{.status.components.%s.installedVersion}", name, name))
	return out
}

// mustReadReleaseManifest parses a testdata ReleaseManifest (path relative to
// the project root) while the spec tree is built, which needs its components.
func mustReadReleaseManifest(path string) *k4allv1alpha1.ReleaseManifest {
	raw, err := os.ReadFile(filepath.Join(root, path))
	if err != nil {
		panic(err)
	}
	rm := &k4allv1alpha1.ReleaseManifest{}
	if err := yaml.Unmarshal(raw, rm); err != nil {
		panic(fmt.Sprintf("%s: %v", path, err))
	}
	return rm
}

// mustReadClusterConfig parses the testdata ClusterConfig the cluster is
// installed with, for the checks that depend on its settings.
func mustReadClusterConfig(path string) *k4allv1alpha1.ClusterConfig {
	raw, err := os.ReadFile(filepath.Join(root, path))
	if err != nil {
		panic(err)
	}
	cc := &k4allv1alpha1.ClusterConfig{}
	if err := yaml.Unmarshal(raw, cc); err != nil {
		panic(fmt.Sprintf("%s: %v", path, err))
	}
	return cc
}

func managed(spec k4allv1alpha1.ComponentSpec) bool {
	return spec.Management != k4allv1alpha1.ComponentManagementRemoved &&
		spec.Management != k4allv1alpha1.ComponentManagementUnmanaged
}

// activeComponents lists, sorted, the components the operator installs from rm.
func activeComponents(rm *k4allv1alpha1.ReleaseManifest) []string {
	var names []string
	for name, spec := range rm.Spec.Components {
		if managed(spec) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// ingressController is the controller serving the k4all routes once rm is installed.
func ingressController(rm *k4allv1alpha1.ReleaseManifest) string {
	for _, name := range []string{"traefik", "ingress-nginx"} {
		if spec, ok := rm.Spec.Components[name]; ok && managed(spec) {
			return strings.TrimPrefix(name, "ingress-")
		}
	}
	return ""
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

// ── Ingress routes ──────────────────────────────────────────────────

const (
	routesFile      = "test/e2e/testdata/ingress-routes.yaml"
	routesNamespace = "k4all-e2e-routes"
)

// defaultCertSubject is the certificate each controller serves when it
// terminates TLS for a host without a secret of its own.
var defaultCertSubject = map[string]string{
	"nginx":   "Kubernetes Ingress Controller Fake Certificate",
	"traefik": "TRAEFIK DEFAULT CERT",
}

func certSubject(cn string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)subject:.*CN ?= ?` + regexp.QuoteMeta(cn))
}

func nodeInternalIP() string {
	return kubectlMustSucceed("get", "nodes",
		"-o", `jsonpath={.items[0].status.addresses[?(@.type=="InternalIP")].address}`)
}

// dedicatedIP is the ingress.*.dedicatedIP controller is published on, or ""
// when it listens on the node IP.
func dedicatedIP(controller string) string {
	if controller == traefikController {
		return expectedTraefik().DedicatedIP
	}
	return mustReadClusterConfig(clusterConfigFile).Spec.Ingress.NginxSettings().DedicatedIP
}

// expectedTraefik is ingress.traefik as the operator has it from k4all
// v2.4.0: the one of the testdata ClusterConfig, or else its ingress.nginx,
// which the operator moves there.
func expectedTraefik() k4allv1alpha1.TraefikIngressConfig {
	ingress := mustReadClusterConfig(clusterConfigFile).Spec.Ingress
	if ingress.Traefik != nil {
		return *ingress.Traefik
	}
	return k4allv1alpha1.TraefikIngressConfig(ingress.NginxSettings())
}

// routesIP is the address the k4all routes are published on through controller.
func routesIP(controller string) string {
	if ip := dedicatedIP(controller); ip != "" {
		return ip
	}
	return nodeInternalIP()
}

// expectTraefikOnDedicatedIP checks that MetalLB gives the Traefik service ip,
// the address a ClusterConfig with ingress.traefik.dedicatedIP asks for.
func expectTraefikOnDedicatedIP(ip string) {
	Eventually(func() string {
		out, _ := kubectl("get", "svc", "-n", "traefik", "-l", "k4all-ingress=traefik",
			"-o", "jsonpath={.items[*].status.loadBalancer.ingress[*].ip}")
		return out
	}, 5*time.Minute, pollInterval).Should(Equal(ip), "Traefik service has no LoadBalancer IP from MetalLB")
}

// routeRequest runs curl in the fixture's client pod against host, resolved
// to ip, the address k4all routes are published on.
func routeRequest(ip, scheme, host string, curlArgs ...string) (string, error) {
	port := "443"
	if scheme == "http" {
		port = "80"
	}
	args := append([]string{"exec", "-n", routesNamespace, "client", "--",
		"curl", "-sS", "--max-time", "10", "--resolve", host + ":" + port + ":" + ip}, curlArgs...)
	return kubectl(append(args, scheme+"://"+host+"/")...)
}

// expectRoutesServedBy checks the three k4all route shapes (testdata/ingress-routes.yaml)
// through controller ("nginx" or "traefik").
func expectRoutesServedBy(controller string) {
	By("deploying the k4all route shapes")
	// The operator reports a Helm component Installed without waiting for
	// its pods: ingress-nginx's admission webhook may refuse for a while.
	Eventually(func() error {
		out, err := kubectl("apply", "-f", routesFile)
		if err != nil {
			return fmt.Errorf("%w: %s", err, out)
		}
		return nil
	}, 3*time.Minute, pollInterval).Should(Succeed())
	for _, name := range []string{"whoami-tls", "whoami-http"} {
		Eventually(func() bool {
			return deploymentAvailable(name, routesNamespace)
		}, 5*time.Minute, pollInterval).Should(BeTrue(), "deployment %s not available", name)
	}
	Eventually(func() string {
		out, _ := kubectl("get", "pod", "client", "-n", routesNamespace, "-o", "jsonpath={.status.phase}")
		return out
	}, 3*time.Minute, pollInterval).Should(Equal("Running"))
	ip := routesIP(controller)

	By("http-backend (headlamp shape): plain HTTP is redirected to HTTPS")
	Eventually(func(g Gomega) {
		out, err := routeRequest(ip, "http", "http-backend.k4all.test",
			"-o", "/dev/null", "-w", "%{http_code} %{redirect_url}")
		g.Expect(err).NotTo(HaveOccurred(), out)
		g.Expect(out).To(MatchRegexp(`^30[18] https://http-backend\.k4all\.test/$`))
	}, 5*time.Minute, pollInterval).Should(Succeed())

	By("http-backend: HTTPS reaches the HTTP backend")
	Eventually(func(g Gomega) {
		out, err := routeRequest(ip, "https", "http-backend.k4all.test", "-k")
		g.Expect(err).NotTo(HaveOccurred(), out)
		g.Expect(out).To(ContainSubstring("Hostname: whoami-http"))
	}, 2*time.Minute, pollInterval).Should(Succeed())

	By("https-backend (argocd shape): TLS terminated by " + controller + ", HTTPS to the backend")
	Eventually(func(g Gomega) {
		out, err := routeRequest(ip, "https", "https-backend.k4all.test", "-kv")
		g.Expect(err).NotTo(HaveOccurred(), out)
		g.Expect(out).To(ContainSubstring("Hostname: whoami-tls"))
		g.Expect(out).To(MatchRegexp(certSubject(defaultCertSubject[controller]).String()))
	}, 2*time.Minute, pollInterval).Should(Succeed())

	By("passthrough (cdi-uploadproxy shape): the backend's own certificate reaches the client")
	Eventually(func(g Gomega) {
		out, err := routeRequest(ip, "https", "passthrough.k4all.test", "-kv")
		g.Expect(err).NotTo(HaveOccurred(), out)
		g.Expect(out).To(ContainSubstring("Hostname: whoami-tls"))
		g.Expect(out).To(MatchRegexp(certSubject("passthrough.k4all.test").String()))
	}, 2*time.Minute, pollInterval).Should(Succeed())
}

// expectNginxHandedOverToTraefik checks what the switch must leave behind for
// the routes existing clusters already have.
func expectNginxHandedOverToTraefik() {
	By("keeping the nginx IngressClass the routes refer to")
	Eventually(func() string {
		out, _ := kubectl("get", "ingressclass", "nginx", "-o", "jsonpath={.spec.controller}")
		return out
	}, 5*time.Minute, pollInterval).Should(Equal("k8s.io/ingress-nginx"))

	By("making Traefik's own class the default instead, if ingress.traefik.isDefault")
	const isDefault = `{.metadata.annotations.ingressclass\.kubernetes\.io/is-default-class}`
	defaults := kubectlMustSucceed("get", "ingressclass", "-o",
		`jsonpath={range .items[*]}{.metadata.name}=`+isDefault+`{" "}{end}`)
	if expectedTraefik().IsDefault {
		Expect(defaults).To(ContainSubstring("traefik=true"))
	} else {
		Expect(defaults).NotTo(ContainSubstring("traefik=true"))
	}
	Expect(defaults).NotTo(ContainSubstring("nginx=true"))

	By("labelling the Traefik service for check-ip.sh")
	Expect(kubectlMustSucceed("get", "svc", "-n", "traefik", "-l", "k4all-ingress=traefik",
		"-o", "jsonpath={.items[*].metadata.name}")).NotTo(BeEmpty())

	By("removing ingress-nginx and its admission webhook")
	Eventually(func() string {
		out, _ := kubectl("get", "pods", "-n", "ingress-nginx", "-o", "jsonpath={.items[*].metadata.name}")
		return out
	}, 5*time.Minute, pollInterval).Should(BeEmpty())
	out, err := kubectl("get", "validatingwebhookconfiguration", "ingress-nginx-admission")
	Expect(err).To(HaveOccurred(), "admission webhook still there: %s", out)

	By("deleting the ingress-nginx namespace, with what the release left outside it")
	Eventually(func() error {
		_, err := kubectl("get", "namespace", "ingress-nginx")
		return err
	}, 5*time.Minute, pollInterval).Should(HaveOccurred(), func() string {
		left, _ := kubectl("get", "all,secrets,configmaps,serviceaccounts,leases,jobs", "-n", "ingress-nginx")
		return "namespace ingress-nginx kept, holding:\n" + left
	})

	By("accepting Ingress updates, as update-routes.sh makes them")
	kubectlMustSucceed("annotate", "ingress", "passthrough", "-n", routesNamespace,
		"k4all-e2e/touched="+time.Now().Format("150405"), "--overwrite")
}

// expectNginxSettingsMovedToTraefik checks the ClusterConfig migration of
// k4all v2.4.0: ingress.traefik set, ingress.nginx gone.
func expectNginxSettingsMovedToTraefik() {
	want := expectedTraefik()
	Eventually(func(g Gomega) {
		out, err := kubectl("get", "clusterconfig", clusterConfigName, "-o", "json")
		g.Expect(err).NotTo(HaveOccurred(), out)
		cc := &k4allv1alpha1.ClusterConfig{}
		g.Expect(json.Unmarshal([]byte(out), cc)).To(Succeed())
		g.Expect(cc.Spec.Ingress.Nginx).To(BeNil(), "ingress.nginx is still there")
		g.Expect(cc.Spec.Ingress.Traefik).To(Equal(&want))
		// 1 moves the settings at the sync, 2 (the ingress-nginx namespace)
		// follows once ingress-nginx is uninstalled.
		g.Expect(cc.Annotations).To(HaveKeyWithValue("k4all.magesgate.com/migrations", "2"))
	}, 5*time.Minute, pollInterval).Should(Succeed())
}

// installsHeadlamp says whether rm installs the dashboard.
func installsHeadlamp(rm *k4allv1alpha1.ReleaseManifest) bool {
	spec, ok := rm.Spec.Components["headlamp"]
	return ok && managed(spec)
}

// expectDashboardServedBy checks the dashboard Ingress of the operator's
// headlamp hook: on controller's class, and answering through it. Through
// Traefik plain HTTP goes to HTTPS (the Middleware traefik/redirect-https).
// It needs the client pod of expectRoutesServedBy.
func expectDashboardServedBy(controller string) {
	ip := routesIP(controller)
	var host string
	By("publishing the dashboard on the " + controller + " class")
	Eventually(func(g Gomega) {
		out, err := kubectl("get", "ingress", "headlamp", "-n", "headlamp", "-o",
			`jsonpath={.spec.ingressClassName} {.metadata.annotations.k4all\.magesgate\.com/dashboard-host}`)
		g.Expect(err).NotTo(HaveOccurred(), out)
		class, owned, _ := strings.Cut(out, " ")
		g.Expect(class).To(Equal(controller))
		host = owned
		g.Expect(host).NotTo(BeEmpty())
	}, 5*time.Minute, pollInterval).Should(Succeed())

	if controller != traefikController {
		By("dashboard: HTTP reaches headlamp")
		Eventually(func(g Gomega) {
			out, err := routeRequest(ip, "http", host, "-o", "/dev/null", "-w", "%{http_code}")
			g.Expect(err).NotTo(HaveOccurred(), out)
			g.Expect(out).To(Equal("200"))
		}, 5*time.Minute, pollInterval).Should(Succeed())
		return
	}

	Expect(host).To(Equal("dashboard." + ip + ".nip.io"))
	By("dashboard: plain HTTP is redirected to HTTPS")
	Eventually(func(g Gomega) {
		out, err := routeRequest(ip, "http", host, "-o", "/dev/null", "-w", "%{http_code} %{redirect_url}")
		g.Expect(err).NotTo(HaveOccurred(), out)
		g.Expect(out).To(MatchRegexp(`^30[18] https://` + regexp.QuoteMeta(host) + `/$`))
	}, 5*time.Minute, pollInterval).Should(Succeed())
	By("dashboard: HTTPS reaches headlamp")
	Eventually(func(g Gomega) {
		out, err := routeRequest(ip, "https", host, "-k", "-o", "/dev/null", "-w", "%{http_code}")
		g.Expect(err).NotTo(HaveOccurred(), out)
		g.Expect(out).To(Equal("200"))
	}, 2*time.Minute, pollInterval).Should(Succeed())
}

// upgradeTo applies the ReleaseManifest in file over from and waits for every
// component to reach its state in to.
func upgradeTo(file string, from, to *k4allv1alpha1.ReleaseManifest) {
	By("applying " + file)
	start := time.Now()
	kubectlMustSucceed("apply", "-f", file)

	reached := map[string]time.Duration{}
	Eventually(func() []string {
		var pending []string
		for name, comp := range to.Spec.Components {
			if _, done := reached[name]; done {
				continue
			}
			want := "Installed/" + comp.Version
			if comp.Management == k4allv1alpha1.ComponentManagementRemoved {
				want = "Removed/"
			}
			if componentStatus(name) == want {
				reached[name] = time.Since(start).Round(time.Second)
				_, _ = fmt.Fprintf(GinkgoWriter, "upgrade: %s %s after %s\n", name, want, reached[name])
				continue
			}
			pending = append(pending, fmt.Sprintf("%s=%s (%s)", name, componentStatus(name), componentMessage(name)))
		}
		return pending
	}, 20*time.Minute, pollInterval).Should(BeEmpty(), "components not at their target state")

	certManager := to.Spec.Components["cert-manager"]
	if certManager.Version != from.Spec.Components["cert-manager"].Version {
		By("checking that cert-manager ran every intermediate step")
		// Every helm upgrade rolls a new controller ReplicaSet; old ones keep their image.
		images := kubectlMustSucceed("get", "replicasets", "-n", "cert-manager",
			"-l", "app.kubernetes.io/name=cert-manager,app.kubernetes.io/component=controller",
			"-o", "jsonpath={.items[*].spec.template.spec.containers[0].image}")
		for _, step := range certManager.UpgradePath {
			Expect(images).To(ContainSubstring(":"+step.Version), "controller ReplicaSet images: %s", images)
		}
	}

	By("waiting for the ReleaseManifest to be Ready again")
	Eventually(func() string {
		out, _ := kubectl("get", "releasemanifest", releaseManifestName,
			"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
		return out
	}, testTimeout, pollInterval).Should(Equal("True"))
	Eventually(func() bool {
		return deploymentReady("cert-manager-webhook", "cert-manager")
	}, testTimeout, pollInterval).Should(BeTrue())
}

var _ = Describe("K4All Operator", Ordered, func() {
	initial := mustReadReleaseManifest(releaseManifestFile)
	expectedComponents := activeComponents(initial)

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
			It(fmt.Sprintf("should have CRD %s", crd), func() {
				out := kubectlMustSucceed("get", "crd", crd)
				Expect(out).To(ContainSubstring(crd))
			})
		}
	})

	// ── Custom Resource Creation ────────────────────────────────────────

	Context("Custom Resources", func() {
		It("should accept a ClusterConfig CR", func() {
			out, err := kubectl("apply", "-f", clusterConfigFile)
			Expect(err).NotTo(HaveOccurred(), "apply ClusterConfig: %s", out)
		})

		It("should accept a ReleaseManifest CR", func() {
			out, err := kubectl("apply", "-f", releaseManifestFile)
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

		// Both testdata ClusterConfigs set componentOverrides for cert-manager:
		// a podLabels value and a --set helmArg, each tagging one Deployment.
		It("should apply ClusterConfig componentOverrides to the cert-manager release", func() {
			labelOf := func(deployment string) string {
				out, _ := kubectl("get", "deployment", deployment, "-n", "cert-manager",
					"-o", "jsonpath={.spec.template.metadata.labels.k4all-e2e}")
				return out
			}
			Expect(labelOf("cert-manager")).To(Equal("values"),
				"componentOverrides.cert-manager.values not applied")
			Expect(labelOf("cert-manager-webhook")).To(Equal("helmargs"),
				"componentOverrides.cert-manager.helmArgs not applied")
		})
	})

	// ── ClusterConfig Status ────────────────────────────────────────────

	// ── Ingress routes ──────────────────────────────────────────────────

	Context("Ingress routes", func() {
		if ip := dedicatedIP(traefikController); ingressController(initial) == traefikController && ip != "" {
			It("should give Traefik its dedicated IP through MetalLB", func() {
				expectTraefikOnDedicatedIP(ip)
			})
		}
		It("should serve the k4all route shapes", func() {
			controller := ingressController(initial)
			if controller == "" {
				Skip(releaseManifestFile + " installs no ingress controller")
			}
			expectRoutesServedBy(controller)
		})
		if controller := ingressController(initial); controller != "" && installsHeadlamp(initial) {
			It("should serve the dashboard through "+controller, func() {
				expectDashboardServedBy(controller)
			})
		}
	})

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

	// ── Upgrade through the next release sets ───────────────────────────

	// Each file in E2E_UPGRADE_TO replaces the installed set, as the
	// ReleaseManifest sync of the matching k4all release does.
	if len(upgradeChain) == 0 {
		Context("Upgrade to the next release set", func() {
			It("should bring every component to its new version through upgradePath", func() {
				Skip("E2E_UPGRADE_TO not set: fresh install only")
			})
		})
	}
	previous := initial
	for _, file := range upgradeChain {
		from, to := previous, mustReadReleaseManifest(file)
		previous = to
		Context("Upgrade to "+file, func() {
			It("should bring every component to its new version through upgradePath", func() {
				upgradeTo(file, from, to)
			})
			if ingressController(from) == "nginx" && ingressController(to) == traefikController {
				It("should move ingress.nginx to ingress.traefik", expectNginxSettingsMovedToTraefik)
				It("should hand the nginx routes over to Traefik", expectNginxHandedOverToTraefik)
				if ip := dedicatedIP(traefikController); ip != "" {
					It("should give Traefik the dedicated IP through MetalLB", func() {
						expectTraefikOnDedicatedIP(ip)
					})
				}
			}
			if controller := ingressController(to); controller != "" {
				It("should serve the k4all route shapes through "+controller, func() {
					expectRoutesServedBy(controller)
				})
				if installsHeadlamp(to) {
					It("should serve the dashboard through "+controller, func() {
						expectDashboardServedBy(controller)
					})
				}
			}
		})
	}

	// ── Diagnostics ─────────────────────────────────────────────────────

	AfterAll(func() {
		By("capturing final ReleaseManifest status for diagnostics")
		out, _ := kubectl("get", "releasemanifest", releaseManifestName,
			"-o", "jsonpath={.status}")
		if out != "" {
			var status map[string]interface{}
			if err := json.Unmarshal([]byte(out), &status); err == nil {
				formatted, _ := json.MarshalIndent(status, "", "  ")
				_, _ = fmt.Fprintf(GinkgoWriter, "\n=== ReleaseManifest Status ===\n%s\n", formatted)
			}
		}

		By("capturing operator logs for diagnostics")
		logs, _ := kubectl("logs", "-n", operatorNamespace,
			"-l", "control-plane=controller-manager",
			"--tail=200")
		_, _ = fmt.Fprintf(GinkgoWriter, "\n=== Operator Logs (last 200 lines) ===\n%s\n", logs)

		By("capturing pod status across namespaces")
		for _, ns := range []string{operatorNamespace, "tigera-operator", "cert-manager", "kube-system",
			"metallb-system", "ingress-nginx", "traefik", routesNamespace} {
			pods, _ := kubectl("get", "pods", "-n", ns, "-o", "wide")
			_, _ = fmt.Fprintf(GinkgoWriter, "\n=== Pods in %s ===\n%s\n", ns, pods)
		}

		By("capturing the ingress services and MetalLB pools for diagnostics")
		for _, args := range [][]string{
			{"get", "svc", "-n", "traefik", "-o", "wide"},
			{"get", "ipaddresspools,l2advertisements", "-n", "metallb-system", "-o", "yaml"},
		} {
			out, _ := kubectl(args...)
			_, _ = fmt.Fprintf(GinkgoWriter, "\n=== kubectl %s ===\n%s\n", strings.Join(args, " "), out)
		}

		By("capturing ingress controller logs for diagnostics")
		for ns, selector := range map[string]string{
			"ingress-nginx": "app.kubernetes.io/name=ingress-nginx",
			"traefik":       "app.kubernetes.io/name=traefik",
		} {
			logs, _ := kubectl("logs", "-n", ns, "-l", selector, "--tail=100")
			_, _ = fmt.Fprintf(GinkgoWriter, "\n=== Logs in %s (last 100 lines) ===\n%s\n", ns, logs)
		}
	})
})
