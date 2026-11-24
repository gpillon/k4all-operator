package controller

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// A real API server (envtest) for tests that need server-side apply, which
// the fake client does not support. Started on first use, shared by the
// package, stopped in TestMain.
var (
	envOnce sync.Once
	testEnv *envtest.Environment
	envCfg  *rest.Config
	envErr  error
)

func envtestConfig(t *testing.T) *rest.Config {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set: run through `make test`")
	}
	envOnce.Do(func() {
		testEnv = &envtest.Environment{
			CRDDirectoryPaths: []string{
				filepath.Join("..", "..", "config", "crd", "bases"),
				// KubeVirt and CDI, cut down (feature_gates_test.go).
				filepath.Join("..", "..", "test", "crds"),
			},
			ErrorIfCRDPathMissing: true,
		}
		envCfg, envErr = testEnv.Start()
	})
	if envErr != nil {
		t.Fatalf("start envtest: %v", envErr)
	}
	return envCfg
}

func TestMain(m *testing.M) {
	// Every Reconcile of the package's tests fails on a ClusterConfig
	// condition the release reconcile sets without listing it in
	// releaseConditionTypes (the operator only logs it).
	unlistedConditionsHook = func(err error) {
		panic("a test's release reconcile: " + err.Error())
	}
	code := m.Run()
	if testEnv != nil {
		_ = testEnv.Stop()
	}
	os.Exit(code)
}

// envtestClient is a client of the shared envtest API server.
func envtestClient(t *testing.T) client.Client {
	t.Helper()
	c, err := client.New(envtestConfig(t), client.Options{Scheme: gateScheme(t)})
	if err != nil {
		t.Fatal(err)
	}
	return c
}
