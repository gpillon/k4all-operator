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
	"fmt"
	"os/exec"
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
	operatorNamespace = "k4all-operator-system"
	testTimeout       = 10 * time.Minute
	pollInterval      = 10 * time.Second
)

func kubectl(args ...string) (string, error) {
	cmd := exec.Command("kubectl", args...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

var _ = Describe("K4All Operator", Ordered, func() {

	BeforeAll(func() {
		By("verifying the cluster is reachable")
		Eventually(func() error {
			_, err := kubectl("cluster-info")
			return err
		}, 2*time.Minute, 5*time.Second).Should(Succeed())
	})

	Context("Operator deployment", func() {
		It("should deploy the operator successfully", func() {
			By("applying the operator manifests")
			_, err := kubectl("apply", "-f", "dist/install.yaml")
			Expect(err).NotTo(HaveOccurred())

			By("waiting for the operator deployment to be available")
			Eventually(func() string {
				out, _ := kubectl("get", "deployment",
					"k4all-operator-controller-manager",
					"-n", operatorNamespace,
					"-o", "jsonpath={.status.conditions[?(@.type=='Available')].status}")
				return out
			}, 3*time.Minute, pollInterval).Should(Equal("True"))
		})
	})

	Context("CRD creation", func() {
		It("should have the ReleaseManifest CRD registered", func() {
			out, err := kubectl("get", "crd", "releasemanifests.k4all.magesgate.com")
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring("releasemanifests.k4all.magesgate.com"))
		})

		It("should have the ClusterConfig CRD registered", func() {
			out, err := kubectl("get", "crd", "clusterconfigs.k4all.magesgate.com")
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring("clusterconfigs.k4all.magesgate.com"))
		})
	})

	Context("ReleaseManifest reconciliation", func() {
		It("should create a ClusterConfig CR", func() {
			By("applying a minimal ClusterConfig")
			_, err := kubectl("apply", "-f", "-", "--stdin")
			if err != nil {
				_, err = kubectl("apply", "-f", "config/samples/k4all_v1alpha1_clusterconfig.yaml")
			}
			Expect(err).NotTo(HaveOccurred())
		})

		It("should create a ReleaseManifest CR", func() {
			By("applying a minimal ReleaseManifest for testing")
			_, err := kubectl("apply", "-f", "config/samples/k4all_v1alpha1_releasemanifest.yaml")
			Expect(err).NotTo(HaveOccurred())
		})

		It("should reconcile the ReleaseManifest and report component status", func() {
			Eventually(func() string {
				out, _ := kubectl("get", "releasemanifest", "k4all",
					"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
				return out
			}, testTimeout, pollInterval).ShouldNot(BeEmpty())
		})
	})

	Context("Component verification", func() {
		It("should report component statuses", func() {
			out, err := kubectl("get", "releasemanifest", "k4all",
				"-o", "jsonpath={.status.components}")
			Expect(err).NotTo(HaveOccurred())
			Expect(out).NotTo(BeEmpty())
			fmt.Fprintf(GinkgoWriter, "Component statuses: %s\n", out)
		})
	})
})
