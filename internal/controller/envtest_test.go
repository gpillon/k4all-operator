package controller

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"k8s.io/client-go/rest"
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
			CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
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
	code := m.Run()
	if testEnv != nil {
		_ = testEnv.Stop()
	}
	os.Exit(code)
}
