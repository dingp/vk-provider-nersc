package provider

import (
	"context"
	corev1 "k8s.io/api/core/v1"
	"testing"
)

func TestNumericHostnameEndpointDoesNotAdvertiseAnAgentInternalIP(t *testing.T) {
	p := &NerscProvider{nodeName: "perlmutter-vk", nodeAddress: "10.43.1.2"}
	p.SetNodeAddressAsHostname(true)
	addresses := p.NodeAddresses(context.Background())
	if len(addresses) != 1 || addresses[0].Type != corev1.NodeHostName || addresses[0].Address != "10.43.1.2" {
		t.Fatalf("addresses=%v", addresses)
	}
	if got := VirtualNodeLabels(p.nodeName)["kubernetes.io/hostname"]; got != "perlmutter-vk" {
		t.Fatal(got)
	}
	p.SetNodeAddressAsHostname(false)
	if got := p.NodeAddresses(context.Background())[0].Type; got != corev1.NodeInternalIP {
		t.Fatal(got)
	}
}
