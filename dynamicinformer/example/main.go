package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"

	di "github.com/stakater/operator-utils/dynamicinformer"
)

var (
	configmapsGVR = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}
	secretsGVR    = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}
)

func printCallback(label string) di.EventCallback {
	return di.EventCallback{
		OnAdd: func(obj *unstructured.Unstructured) {
			fmt.Printf("  [%s] ADD    %s/%s\n", label, obj.GetNamespace(), obj.GetName())
		},
		OnUpdate: func(_, newObj *unstructured.Unstructured) {
			fmt.Printf("  [%s] UPDATE %s/%s\n", label, newObj.GetNamespace(), newObj.GetName())
		},
		OnDelete: func(obj *unstructured.Unstructured) {
			fmt.Printf("  [%s] DELETE %s/%s\n", label, obj.GetNamespace(), obj.GetName())
		},
	}
}

func waitForEnter(prompt string) {
	fmt.Printf("\n>>> %s (press Enter to continue)\n", prompt)
	_, _ = bufio.NewReader(os.Stdin).ReadBytes('\n')
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// Connect to cluster using default kubeconfig
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, nil).ClientConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load kubeconfig: %v\n", err)
		fmt.Fprintf(os.Stderr, "Make sure your kubeconfig points to a running cluster.\n")
		fmt.Fprintf(os.Stderr, "  e.g.: kubectl config use-context kind-<cluster-name>\n")
		os.Exit(1)
	}

	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create dynamic client: %v\n", err)
		os.Exit(1)
	}

	registry := di.NewWatchRegistry(dynamicClient, 30*time.Second)
	consumerID := "demo-consumer"
	ns := "default"

	fmt.Println("=== Dynamic Watch Registry Demo ===")
	fmt.Printf("Namespace: %s\n", ns)
	fmt.Println()

	// --- Step 1: Watch ConfigMaps ---
	fmt.Println("Step 1: Watching ConfigMaps in", ns)
	regs, err := registry.EnsureWatchSet(ctx, consumerID, []di.WatchRequest{
		{
			Key:      di.WatchKey{GVR: configmapsGVR, Namespace: ns},
			Callback: printCallback("ConfigMap"),
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "EnsureWatchSet failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  Registered %d watch(es)\n", len(regs))
	fmt.Println("  You should see existing ConfigMaps appear as ADD events.")
	fmt.Println()
	fmt.Println("  Try in another terminal:")
	fmt.Println("    kubectl create configmap demo-cm --from-literal=key=value")
	fmt.Println("    kubectl patch configmap demo-cm -p '{\"data\":{\"key\":\"updated\"}}'")
	fmt.Println("    kubectl delete configmap demo-cm")

	waitForEnter("Step 2: Add Secrets watch (ConfigMaps watch stays)")

	// --- Step 2: Add Secrets watch alongside ConfigMaps ---
	fmt.Println("Step 2: Watching ConfigMaps + Secrets in", ns)
	regs, err = registry.EnsureWatchSet(ctx, consumerID, []di.WatchRequest{
		{
			Key:      di.WatchKey{GVR: configmapsGVR, Namespace: ns},
			Callback: printCallback("ConfigMap"),
		},
		{
			Key:      di.WatchKey{GVR: secretsGVR, Namespace: ns},
			Callback: printCallback("Secret"),
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "EnsureWatchSet failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  Registered %d watch(es)\n", len(regs))
	fmt.Println("  You should see existing Secrets appear as ADD events.")
	fmt.Println()
	fmt.Println("  Try in another terminal:")
	fmt.Println("    kubectl create secret generic demo-secret --from-literal=pw=test")
	fmt.Println("    kubectl delete secret demo-secret")

	waitForEnter("Step 3: Remove ConfigMaps watch (keep only Secrets)")

	// --- Step 3: Remove ConfigMaps, keep only Secrets ---
	fmt.Println("Step 3: Watching only Secrets in", ns, "(ConfigMaps watch removed)")
	regs, err = registry.EnsureWatchSet(ctx, consumerID, []di.WatchRequest{
		{
			Key:      di.WatchKey{GVR: secretsGVR, Namespace: ns},
			Callback: printCallback("Secret"),
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "EnsureWatchSet failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  Registered %d watch(es)\n", len(regs))
	fmt.Println("  ConfigMap events should stop. Secret events continue.")
	fmt.Println()
	fmt.Println("  Try in another terminal:")
	fmt.Println("    kubectl create configmap invisible-cm --from-literal=k=v  # no event expected")
	fmt.Println("    kubectl delete configmap invisible-cm")

	waitForEnter("Step 4: ReleaseAll (stop all watches)")

	// --- Step 4: Release all ---
	fmt.Println("Step 4: Releasing all watches")
	if err := registry.ReleaseAll(consumerID); err != nil {
		fmt.Fprintf(os.Stderr, "ReleaseAll failed: %v\n", err)
	}
	fmt.Println("  All watches released. No more events should appear.")
	fmt.Println()
	fmt.Println("  Try creating resources — nothing should print.")

	waitForEnter("Done. Exiting")
}
