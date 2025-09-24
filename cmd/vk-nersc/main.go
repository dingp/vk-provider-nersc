package main

import (
    "context"
    "log"
    "os"

    corev1 "k8s.io/api/core/v1"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/client-go/kubernetes"
    "k8s.io/client-go/rest"
    "k8s.io/client-go/tools/clientcmd"

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

    // Create Kubernetes client
    config, err := rest.InClusterConfig()
    if err != nil {
        // Fallback to kubeconfig for local development
        kubeconfig := os.Getenv("KUBECONFIG")
        if kubeconfig == "" {
            kubeconfig = os.Getenv("HOME") + "/.kube/config"
        }
        config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
        if err != nil {
            log.Fatalf("Failed to create Kubernetes config: %v", err)
        }
    }

    clientset, err := kubernetes.NewForConfig(config)
    if err != nil {
        log.Fatalf("Failed to create Kubernetes client: %v", err)
    }

    // Create the virtual node
    virtualNode := &corev1.Node{
        ObjectMeta: metav1.ObjectMeta{
            Name: nodeName,
            Labels: map[string]string{
                "type":                   "virtual-kubelet",
                "kubernetes.io/role":     "agent",
                "kubernetes.io/hostname": nodeName,
                "kubernetes.io/os":       "linux",
                "kubernetes.io/arch":     "amd64",
            },
        },
        Spec: corev1.NodeSpec{
            Taints: []corev1.Taint{
                {
                    Key:    "virtual-kubelet.io/provider",
                    Value:  "nersc",
                    Effect: corev1.TaintEffectNoSchedule,
                },
            },
        },
    }

    ctx := context.Background()
    
    // Create and run the virtual kubelet node controller
    nodeController, err := node.NewNodeController(
        prov,
        virtualNode,
        clientset.CoreV1().Nodes(),
    )
    if err != nil {
        log.Fatalf("Failed to create node controller: %v", err)
    }
    
    log.Printf("Starting Virtual Kubelet node %s for Perlmutter...", nodeName)
    if err := nodeController.Run(ctx); err != nil {
        log.Fatalf("VK exited: %v", err)
    }
}
