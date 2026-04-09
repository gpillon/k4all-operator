#!/bin/bash
# E2E test runner for k4all-operator using a containerized k4all instance.
# This starts a k4all bootc container with systemd (KinD-like approach),
# waits for Kubernetes to be ready, then deploys the operator and runs tests.
#
# Prerequisites:
#   - podman or docker
#   - A built k4all-bootc image (e.g. from bootc/Makefile)
#   - A built operator image (e.g. make docker-build)
#
# Usage:
#   ./hack/test-e2e-container.sh [K4ALL_IMAGE] [OPERATOR_IMAGE]

set -euo pipefail

K4ALL_IMAGE="${1:-ghcr.io/gpillon/k4all-bootc:latest}"
OPERATOR_IMAGE="${2:-controller:latest}"
CONTAINER_NAME="k4all-operator-e2e"
CONTAINER_TOOL="${CONTAINER_TOOL:-podman}"

cleanup() {
    echo "=== Cleaning up ==="
    "${CONTAINER_TOOL}" rm -f "${CONTAINER_NAME}" 2>/dev/null || true
}
trap cleanup EXIT

echo "=== Starting k4all test container ==="
"${CONTAINER_TOOL}" rm -f "${CONTAINER_NAME}" 2>/dev/null || true

"${CONTAINER_TOOL}" run -d --name "${CONTAINER_NAME}" \
    --hostname k4all-e2e-node \
    --privileged \
    --security-opt unmask=ALL \
    --tmpfs /run --tmpfs /tmp \
    -v /sys/fs/cgroup:/sys/fs/cgroup:rw \
    -p 6443:6443 \
    "${K4ALL_IMAGE}" /sbin/init

echo "=== Waiting for container to start ==="
sleep 10

echo "=== Setting up k4all node configuration ==="
"${CONTAINER_TOOL}" exec "${CONTAINER_NAME}" bash -c '
    # Set node type
    echo "bootstrap" > /etc/node-type

    # Create default config if needed
    if [ ! -f /etc/k4all-config.json ]; then
        cp /usr/local/share/default-cluster-config.json /etc/k4all-config.json
    fi

    # Ensure release file exists
    if [ ! -f /etc/k4all-release.yaml ]; then
        cp /usr/local/share/k4all-release.yaml /etc/k4all-release.yaml
    fi

    # Touch ph2 done to skip network reboot in container
    mkdir -p /opt/k4all
    touch /opt/k4all/setup-ph2.done

    # Fix DNS for container environment
    if [ -f /etc/resolv.conf ]; then
        if ! grep -q "nameserver" /etc/resolv.conf 2>/dev/null; then
            echo "nameserver 8.8.8.8" >> /etc/resolv.conf
        fi
    fi
'

echo "=== Waiting for kubelet/crio to start ==="
RETRIES=60
while [ $RETRIES -gt 0 ]; do
    if "${CONTAINER_TOOL}" exec "${CONTAINER_NAME}" systemctl is-active crio.service &>/dev/null; then
        echo "CRI-O is active"
        break
    fi
    RETRIES=$((RETRIES - 1))
    sleep 5
done

echo "=== Waiting for Kubernetes API server ==="
RETRIES=120
while [ $RETRIES -gt 0 ]; do
    if "${CONTAINER_TOOL}" exec "${CONTAINER_NAME}" kubectl --kubeconfig=/etc/kubernetes/admin.conf get nodes &>/dev/null 2>&1; then
        echo "API server is ready!"
        break
    fi
    RETRIES=$((RETRIES - 1))
    sleep 5
done

if [ $RETRIES -eq 0 ]; then
    echo "ERROR: Kubernetes API server not ready after timeout"
    echo "=== Container logs ==="
    "${CONTAINER_TOOL}" exec "${CONTAINER_NAME}" journalctl -u kubelet --no-pager -n 50
    exit 1
fi

echo "=== Copying kubeconfig ==="
KUBECONFIG_DIR="${HOME}/.kube"
mkdir -p "${KUBECONFIG_DIR}"
"${CONTAINER_TOOL}" cp "${CONTAINER_NAME}:/etc/kubernetes/admin.conf" "${KUBECONFIG_DIR}/k4all-e2e.conf"

# Fix server address to use localhost (we published port 6443)
sed -i 's|server:.*|server: https://127.0.0.1:6443|' "${KUBECONFIG_DIR}/k4all-e2e.conf"
export KUBECONFIG="${KUBECONFIG_DIR}/k4all-e2e.conf"

echo "=== Loading operator image into container ==="
"${CONTAINER_TOOL}" exec "${CONTAINER_NAME}" bash -c "
    crictl pull ${OPERATOR_IMAGE} 2>/dev/null || true
" || true

echo "=== Building operator installer ==="
make build-installer IMG="${OPERATOR_IMAGE}"

echo "=== Running E2E tests ==="
export KUBECONFIG="${KUBECONFIG_DIR}/k4all-e2e.conf"
go test ./test/e2e/ -v -ginkgo.v -timeout 20m

echo "=== E2E tests complete ==="
