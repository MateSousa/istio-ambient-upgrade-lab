package live

import (
	"os"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// restConfig resolves an in-cluster config first, then falls back to KUBECONFIG
// / ~/.kube/config for out-of-cluster runs (the measure orchestrator can run
// either as a Pod or from a laptop).
func restConfig() (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kc := os.Getenv("KUBECONFIG"); kc != "" {
		rules.ExplicitPath = kc
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		rules, &clientcmd.ConfigOverrides{}).ClientConfig()
}

// NewClients builds a typed clientset and a dynamic client from the resolved
// rest config.
func NewClients() (kubernetes.Interface, dynamic.Interface, error) {
	cs, dyn, _, err := NewClientsAndConfig()
	return cs, dyn, err
}

// NewClientsAndConfig is NewClients plus the resolved *rest.Config. The client
// observer needs the rest config to open a SPDY pod-exec stream (SHOW CLIENTS /
// /hold), which the typed and dynamic clients alone cannot provide.
func NewClientsAndConfig() (kubernetes.Interface, dynamic.Interface, *rest.Config, error) {
	cfg, err := restConfig()
	if err != nil {
		return nil, nil, nil, err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, nil, nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, nil, nil, err
	}
	return cs, dyn, cfg, nil
}
