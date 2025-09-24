package main

import (
    "context"
    "log"
    "os"

    "github.com/virtual-kubelet/virtual-kubelet/node"
    "vk-provider-nersc/pkg/provider"
)

func main() {
    endpoint := os.Getenv("SF_API_ENDPOINT")
    token := os.Getenv("SF_API_TOKEN")
    nodeName := os.Getenv("VK_NODE_NAME")
    if nodeName == "" {
        nodeName = "perlmutter-vk"
    }

    prov, err := provider.NewNerscProvider(endpoint, token, nodeName)
    if err != nil {
        log.Fatalf("Failed to create provider: %v", err)
    }

    vkNode, err := node.NewNode(prov, node.WithNodeName(nodeName))
    if err != nil {
        log.Fatalf("Failed to create VK node: %v", err)
    }

    log.Printf("Starting Virtual Kubelet node %s for Perlmutter...", nodeName)
    if err := vkNode.Run(context.Background()); err != nil {
        log.Fatalf("VK exited: %v", err)
    }
}
