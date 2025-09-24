package main

import (
    "context"
    "log"
    "os"

    "github.com/virtual-kubelet/virtual-kubelet/node"
    "github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"
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

    ctx := context.Background()
    
    // Create node configuration
    cfg := nodeutil.ProviderConfig{
        NodeName: nodeName,
    }

    // Create and run the virtual kubelet node
    vkNode := node.NewNodeController(prov, cfg)
    
    log.Printf("Starting Virtual Kubelet node %s for Perlmutter...", nodeName)
    if err := vkNode.Run(ctx); err != nil {
        log.Fatalf("VK exited: %v", err)
    }
}
