package hooks

import (
	"context"
	"fmt"
	"net"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Settings shared by the ingress controller hooks (ingress-nginx, traefik).
const (
	haTypeKubeVIP           = "kubevip"
	kubeVIPLoadBalancer     = "kube-vip.io/kube-vip-class"
	serviceTypeLoadBalancer = string(corev1.ServiceTypeLoadBalancer)
)

// nodeInternalIP returns the first node InternalIP, the address the ingress
// controller is published on when there is no dedicated IP.
func nodeInternalIP(ctx context.Context, c client.Client) (string, error) {
	nodes := &corev1.NodeList{}
	if err := c.List(ctx, nodes); err != nil {
		return "", fmt.Errorf("list nodes: %w", err)
	}

	for _, node := range nodes.Items {
		for _, addr := range node.Status.Addresses {
			if addr.Type == corev1.NodeInternalIP {
				if ip := net.ParseIP(addr.Address); ip != nil {
					return addr.Address, nil
				}
			}
		}
	}
	return "", fmt.Errorf("no node with InternalIP found")
}
