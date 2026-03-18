package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/maestra-io/netbox-zone-labeler/internal/netbox"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

const (
	zoneLabel       = "topology.kubernetes.io/zone"
	resyncInterval  = 10 * time.Minute
	reconcilePeriod = 30 * time.Minute
)

func main() {
	netboxURL := mustEnv("NETBOX_URL")
	netboxToken := mustEnv("NETBOX_TOKEN")

	slog.Info("starting netbox-zone-labeler",
		"netbox_url", netboxURL,
		"resync_interval", resyncInterval,
		"reconcile_period", reconcilePeriod,
	)

	cfg, err := rest.InClusterConfig()
	if err != nil {
		slog.Error("failed to get in-cluster config", "error", err)
		os.Exit(1)
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		slog.Error("failed to create kubernetes client", "error", err)
		os.Exit(1)
	}

	nb := netbox.NewClient(netboxURL, netboxToken)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	factory := informers.NewSharedInformerFactory(clientset, resyncInterval)
	nodeInformer := factory.Core().V1().Nodes().Informer()

	nodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			node := obj.(*corev1.Node)
			labelNode(ctx, clientset, nb, node)
		},
		UpdateFunc: func(_, newObj interface{}) {
			node := newObj.(*corev1.Node)
			if _, ok := node.Labels[zoneLabel]; !ok {
				labelNode(ctx, clientset, nb, node)
			}
		},
	})

	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())

	slog.Info("informer synced, starting periodic reconcile loop")

	ticker := time.NewTicker(reconcilePeriod)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("shutting down")
			return
		case <-ticker.C:
			reconcileAll(ctx, clientset, nb)
		}
	}
}

func labelNode(ctx context.Context, clientset kubernetes.Interface, nb *netbox.Client, node *corev1.Node) {
	hostname := node.Name

	rack, err := nb.GetDeviceRack(ctx, hostname)
	if err != nil {
		slog.Warn("failed to get rack from netbox", "node", hostname, "error", err)
		return
	}

	rack = sanitizeLabel(rack)

	current, ok := node.Labels[zoneLabel]
	if ok && current == rack {
		return
	}

	patch := fmt.Sprintf(`{"metadata":{"labels":{%q:%q}}}`, zoneLabel, rack)
	_, err = clientset.CoreV1().Nodes().Patch(ctx, hostname, ktypes.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{})
	if err != nil {
		slog.Error("failed to patch node", "node", hostname, "label", rack, "error", err)
		return
	}

	slog.Info("labeled node", "node", hostname, "zone", rack)
}

func reconcileAll(ctx context.Context, clientset kubernetes.Interface, nb *netbox.Client) {
	nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		slog.Error("failed to list nodes", "error", err)
		return
	}

	slog.Info("reconciling all nodes", "count", len(nodes.Items))
	for i := range nodes.Items {
		labelNode(ctx, clientset, nb, &nodes.Items[i])
	}
}

func sanitizeLabel(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, " ", "-")
	return s
}

func mustEnv(key string) string {
	val := os.Getenv(key)
	if val == "" {
		slog.Error("required env var not set", "key", key)
		os.Exit(1)
	}
	return val
}
