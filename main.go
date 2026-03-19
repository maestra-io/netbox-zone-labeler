package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/maestra-io/netbox-zone-labeler/internal/netbox"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	zoneLabel        = "topology.kubernetes.io/zone"
	resyncInterval   = 10 * time.Minute
	reconcilePeriod  = 30 * time.Minute
	negativeCacheTTL = 30 * time.Minute
	healthAddr       = ":8081"
	reconcileDelay   = 100 * time.Millisecond
	maxLabelLength   = 63
)

var (
	nodesLabeled = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "netbox_zone_labeler_nodes_labeled_total",
		Help: "Total number of nodes successfully labeled",
	})
	labelErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "netbox_zone_labeler_errors_total",
		Help: "Total number of labeling errors by reason",
	}, []string{"reason"})
	netboxRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "netbox_zone_labeler_netbox_request_duration_seconds",
		Help:    "NetBox API request duration in seconds",
		Buckets: prometheus.DefBuckets,
	}, []string{"status"})

	labelRegexp = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9._-]*[a-zA-Z0-9])?$`)
)

func main() {
	netboxURL := mustEnv("NETBOX_URL")
	netboxToken := mustEnv("NETBOX_TOKEN")
	excludeRoles := os.Getenv("EXCLUDE_NODE_ROLES")

	slog.Info("starting netbox-zone-labeler",
		"netbox_url", netboxURL,
		"resync_interval", resyncInterval,
		"reconcile_period", reconcilePeriod,
		"exclude_roles", excludeRoles,
	)

	prometheus.MustRegister(nodesLabeled, labelErrors, netboxRequestDuration)

	cfg, err := buildKubeConfig()
	if err != nil {
		slog.Error("failed to get kubernetes config", "error", err)
		os.Exit(1)
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		slog.Error("failed to create kubernetes client", "error", err)
		os.Exit(1)
	}

	nb := netbox.NewClient(netboxURL, netboxToken)
	nc := newNegativeCache(negativeCacheTTL)
	excluded := parseExcludeRoles(excludeRoles)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	var ready atomic.Bool
	go serveHealth(&ready)

	factory := informers.NewSharedInformerFactory(clientset, resyncInterval)
	nodeInformer := factory.Core().V1().Nodes().Informer()

	nodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			node, ok := toNode(obj)
			if !ok || isExcluded(node, excluded) {
				return
			}
			labelNode(ctx, clientset, nb, nc, node)
		},
		UpdateFunc: func(_, newObj interface{}) {
			node, ok := toNode(newObj)
			if !ok {
				return
			}
			if _, hasLabel := node.Labels[zoneLabel]; hasLabel {
				return
			}
			if isExcluded(node, excluded) || nc.Has(node.Name) {
				return
			}
			labelNode(ctx, clientset, nb, nc, node)
		},
	})

	factory.Start(ctx.Done())
	synced := factory.WaitForCacheSync(ctx.Done())
	for _, ok := range synced {
		if !ok {
			slog.Error("informer failed to sync")
			os.Exit(1)
		}
	}
	ready.Store(true)

	slog.Info("informer synced, starting periodic reconcile loop")

	ticker := time.NewTicker(reconcilePeriod)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("shutting down")
			return
		case <-ticker.C:
			nc.Clear()
			reconcileAll(ctx, clientset, nb, nc, excluded)
		}
	}
}

// buildKubeConfig returns a Kubernetes client config, trying in-cluster first
// then falling back to KUBECONFIG or ~/.kube/config for local development.
func buildKubeConfig() (*rest.Config, error) {
	cfg, err := rest.InClusterConfig()
	if err == nil {
		return cfg, nil
	}
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		if home, err := os.UserHomeDir(); err == nil {
			kubeconfig = home + "/.kube/config"
		}
	}
	return clientcmd.BuildConfigFromFlags("", kubeconfig)
}

// toNode safely extracts a *corev1.Node from an informer event object,
// handling both direct types and cache.DeletedFinalStateUnknown wrappers.
func toNode(obj interface{}) (*corev1.Node, bool) {
	if node, ok := obj.(*corev1.Node); ok {
		return node, true
	}
	if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		if node, ok := d.Obj.(*corev1.Node); ok {
			return node, true
		}
	}
	return nil, false
}

// parseExcludeRoles parses a comma-separated list of node roles to exclude.
func parseExcludeRoles(s string) map[string]struct{} {
	roles := make(map[string]struct{})
	if s == "" {
		return roles
	}
	for _, r := range strings.Split(s, ",") {
		r = strings.TrimSpace(r)
		if r != "" {
			roles[r] = struct{}{}
		}
	}
	return roles
}

// isExcluded returns true if the node has any of the excluded roles.
func isExcluded(node *corev1.Node, excludedRoles map[string]struct{}) bool {
	for role := range excludedRoles {
		if _, ok := node.Labels["node-role.kubernetes.io/"+role]; ok {
			return true
		}
	}
	return false
}

// labelNode fetches the rack for a node from NetBox and applies the zone label.
// Nodes not found in NetBox are added to the negative cache.
func labelNode(ctx context.Context, clientset kubernetes.Interface, nb *netbox.Client, nc *negativeCache, node *corev1.Node) {
	hostname := node.Name
	if nc.Has(hostname) {
		return
	}

	start := time.Now()
	rack, err := nb.GetDeviceRack(ctx, hostname)
	duration := time.Since(start).Seconds()

	if err != nil {
		netboxRequestDuration.WithLabelValues("error").Observe(duration)
		if netbox.IsNotFound(err) || netbox.IsNoRack(err) {
			nc.Set(hostname)
			slog.Debug("device not in netbox or has no rack, cached", "node", hostname)
		} else {
			slog.Warn("failed to get rack from netbox", "node", hostname, "error", err)
			labelErrors.WithLabelValues("netbox").Inc()
		}
		return
	}
	netboxRequestDuration.WithLabelValues("success").Observe(duration)

	rack = sanitizeLabel(rack)
	if !isValidLabel(rack) {
		slog.Error("invalid label value from netbox", "node", hostname, "rack", rack)
		labelErrors.WithLabelValues("invalid_label").Inc()
		return
	}

	current, ok := node.Labels[zoneLabel]
	if ok && current == rack {
		return
	}

	patch, _ := json.Marshal(map[string]interface{}{
		"metadata": map[string]interface{}{
			"labels": map[string]string{
				zoneLabel: rack,
			},
		},
	})

	_, err = clientset.CoreV1().Nodes().Patch(ctx, hostname, ktypes.StrategicMergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		slog.Error("failed to patch node", "node", hostname, "label", rack, "error", err)
		labelErrors.WithLabelValues("patch").Inc()
		return
	}

	nodesLabeled.Inc()
	slog.Info("labeled node", "node", hostname, "zone", rack)
}

// reconcileAll iterates all nodes and ensures each one has the correct zone label.
func reconcileAll(ctx context.Context, clientset kubernetes.Interface, nb *netbox.Client, nc *negativeCache, excluded map[string]struct{}) {
	nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		slog.Error("failed to list nodes", "error", err)
		return
	}

	slog.Info("reconciling all nodes", "count", len(nodes.Items))
	for i := range nodes.Items {
		if ctx.Err() != nil {
			return
		}
		if isExcluded(&nodes.Items[i], excluded) {
			continue
		}
		labelNode(ctx, clientset, nb, nc, &nodes.Items[i])
		time.Sleep(reconcileDelay)
	}
}

// sanitizeLabel normalizes a string for use as a Kubernetes label value.
func sanitizeLabel(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, " ", "-")
	return s
}

// isValidLabel checks if a string is a valid Kubernetes label value.
func isValidLabel(s string) bool {
	if len(s) == 0 || len(s) > maxLabelLength {
		return false
	}
	return labelRegexp.MatchString(s)
}

// serveHealth starts the HTTP server for health probes and Prometheus metrics.
func serveHealth(ready *atomic.Bool) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if ready.Load() {
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "ok")
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, "not ready")
		}
	})
	mux.Handle("/metrics", promhttp.Handler())
	server := &http.Server{
		Addr:              healthAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("health/metrics server failed", "error", err)
		os.Exit(1)
	}
}

// negativeCache stores hostnames not found in NetBox to avoid repeated lookups.
type negativeCache struct {
	mu    sync.RWMutex
	items map[string]time.Time
	ttl   time.Duration
}

// newNegativeCache creates a cache that remembers missed lookups for the given TTL.
func newNegativeCache(ttl time.Duration) *negativeCache {
	return &negativeCache{
		items: make(map[string]time.Time),
		ttl:   ttl,
	}
}

// Set adds a key to the negative cache with the current timestamp.
func (c *negativeCache) Set(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[key] = time.Now()
}

// Has returns true if the key is in the cache and hasn't expired.
func (c *negativeCache) Has(key string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	t, ok := c.items[key]
	if !ok {
		return false
	}
	return time.Since(t) < c.ttl
}

// Clear removes all entries from the cache.
func (c *negativeCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = make(map[string]time.Time)
}

// mustEnv returns the value of the given environment variable or exits if unset.
func mustEnv(key string) string {
	val := os.Getenv(key)
	if val == "" {
		slog.Error("required env var not set", "key", key)
		os.Exit(1)
	}
	return val
}
