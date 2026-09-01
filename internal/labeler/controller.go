// Package labeler keeps the topology.kubernetes.io/zone label of every node
// equal to the NetBox rack of the machine it runs on.
//
// It is a plain controller: a metadata-only informer on Nodes feeds a
// rate-limited work queue, one worker resolves the rack and patches the label,
// and a ticker re-queues every node once per period so drift in NetBox is
// picked up. Nodes NetBox knows nothing about are retried on that full pass
// only; transient failures are retried with exponential backoff.
package labeler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/metadata/metadatainformer"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/maestra-io/netbox-zone-labeler/internal/netbox"
)

const fieldManager = "netbox-zone-labeler"

var nodesGVR = schema.GroupVersionResource{Version: "v1", Resource: "nodes"}

// Options configure a Controller.
type Options struct {
	Client       metadata.Interface
	Lookup       netbox.RackLookup
	ExcludeRoles []string
	// Period between full passes over all nodes.
	Period time.Duration
	// DryRun logs what would be patched and patches nothing.
	DryRun     bool
	Registerer prometheus.Registerer
	Logger     *slog.Logger
	// RateLimiter for retries of transient failures; nil means the client-go
	// default (5ms doubling up to 1000s, plus a 10 qps bucket).
	RateLimiter workqueue.TypedRateLimiter[string]
}

// Controller labels nodes. Create it with New and drive it with Run.
type Controller struct {
	nodes    metadata.ResourceInterface
	informer cache.SharedIndexInformer
	queue    workqueue.TypedRateLimitingInterface[string]
	lookup   netbox.RackLookup
	excluded map[string]struct{}
	period   time.Duration
	dryRun   bool
	m        *metrics
	log      *slog.Logger

	synced   atomic.Bool
	lastPass atomic.Int64 // unix seconds
}

// New wires the informer, the queue and the metrics. Nothing runs until Run.
func New(o Options) (*Controller, error) {
	if o.Client == nil || o.Lookup == nil {
		return nil, errors.New("labeler: Client and Lookup are required")
	}
	if o.Period <= 0 {
		return nil, errors.New("labeler: Period must be positive")
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Registerer == nil {
		o.Registerer = prometheus.DefaultRegisterer
	}
	if o.RateLimiter == nil {
		o.RateLimiter = workqueue.DefaultTypedControllerRateLimiter[string]()
	}

	c := &Controller{
		nodes:    o.Client.Resource(nodesGVR),
		informer: metadatainformer.NewSharedInformerFactory(o.Client, 0).ForResource(nodesGVR).Informer(),
		queue: workqueue.NewTypedRateLimitingQueueWithConfig(o.RateLimiter,
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "nodes"}),
		lookup:   o.Lookup,
		excluded: make(map[string]struct{}, len(o.ExcludeRoles)),
		period:   o.Period,
		dryRun:   o.DryRun,
		log:      o.Logger,
	}
	for _, r := range o.ExcludeRoles {
		c.excluded["node-role.kubernetes.io/"+r] = struct{}{}
	}
	c.m = newMetrics(o.Registerer, c.queue.Len)

	if _, err := c.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.enqueue,
		UpdateFunc: c.onUpdate,
	}); err != nil {
		return nil, fmt.Errorf("labeler: add event handler: %w", err)
	}
	return c, nil
}

// Ready reports whether the informer has completed its initial list.
func (c *Controller) Ready() bool { return c.synced.Load() }

// Healthy reports whether the full-pass ticker is alive: false when no pass
// has been scheduled for three periods.
func (c *Controller) Healthy() bool {
	last := c.lastPass.Load()
	return last == 0 || time.Since(time.Unix(last, 0)) < 3*c.period
}

// Run blocks until ctx is cancelled or the informer fails to sync.
func (c *Controller) Run(ctx context.Context) error {
	defer c.queue.ShutDown()

	go c.informer.Run(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), c.informer.HasSynced) {
		return errors.New("labeler: node informer did not sync")
	}
	c.synced.Store(true)
	c.markPass()
	c.log.Info("node informer synced", "nodes", len(c.informer.GetStore().ListKeys()), "period", c.period.String())

	go c.worker(ctx)

	ticker := time.NewTicker(c.period)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			c.log.Info("shutting down")
			return nil
		case <-ticker.C:
			c.fullPass()
		}
	}
}

func (c *Controller) enqueue(obj any) {
	if m, ok := obj.(*metav1.PartialObjectMetadata); ok {
		c.queue.Add(m.Name)
	}
}

// onUpdate re-queues a node only when its labels changed. Kubelet bumps the
// node's resourceVersion on every status heartbeat, and those must not turn
// into NetBox lookups.
func (c *Controller) onUpdate(oldObj, newObj any) {
	o, ok := oldObj.(*metav1.PartialObjectMetadata)
	if !ok {
		return
	}
	n, ok := newObj.(*metav1.PartialObjectMetadata)
	if !ok {
		return
	}
	if !maps.Equal(o.Labels, n.Labels) {
		c.queue.Add(n.Name)
	}
}

func (c *Controller) fullPass() {
	keys := c.informer.GetStore().ListKeys()
	for _, k := range keys {
		c.queue.Add(k)
	}
	c.markPass()
	c.log.Info("full pass scheduled", "nodes", len(keys))
}

func (c *Controller) markPass() {
	now := time.Now()
	c.lastPass.Store(now.Unix())
	c.m.lastPass.Set(float64(now.Unix()))
}

func (c *Controller) worker(ctx context.Context) {
	for {
		key, quit := c.queue.Get()
		if quit {
			return
		}
		switch err := c.sync(ctx, key); {
		case err == nil || ctx.Err() != nil:
			c.queue.Forget(key)
		default:
			c.queue.AddRateLimited(key)
		}
		c.queue.Done(key)
		c.m.withoutZone.Set(float64(c.countWithoutZone()))
	}
}

// sync brings one node to the desired state. It returns an error only for
// transient failures worth a rate-limited retry; permanent outcomes (node
// unknown to NetBox, unusable rack name) are logged and left for the next
// full pass.
func (c *Controller) sync(ctx context.Context, key string) error {
	obj, exists, err := c.informer.GetStore().GetByKey(key)
	if err != nil || !exists {
		return nil
	}
	node, ok := obj.(*metav1.PartialObjectMetadata)
	if !ok || c.isExcluded(node) {
		return nil
	}
	log := c.log.With("node", node.Name)

	start := time.Now()
	rack, err := c.lookup.LookupRack(ctx, node.Name)
	elapsed := time.Since(start).Seconds()
	switch {
	case errors.Is(err, netbox.ErrNoZone):
		c.m.lookup.WithLabelValues("miss").Observe(elapsed)
		log.Info("no zone in netbox, retrying on the next full pass", "reason", err)
		return nil
	case errors.Is(err, netbox.ErrAmbiguous):
		c.m.lookup.WithLabelValues("error").Observe(elapsed)
		c.m.errors.WithLabelValues("ambiguous").Inc()
		log.Error("ambiguous name in netbox, fix the inventory", "error", err)
		return nil
	case err != nil:
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c.m.lookup.WithLabelValues("error").Observe(elapsed)
		c.m.errors.WithLabelValues("netbox").Inc()
		log.Warn("netbox lookup failed, retrying with backoff", "error", err)
		return err
	}
	c.m.lookup.WithLabelValues("found").Observe(elapsed)

	zone := ZoneFromRack(rack)
	if err := ValidateZone(zone); err != nil {
		c.m.errors.WithLabelValues("invalid_label").Inc()
		log.Error("rack name is not a valid label value", "rack", rack, "zone", zone, "error", err)
		return nil
	}

	current := node.Labels[ZoneLabel]
	if current == zone {
		log.Debug("zone up to date", "zone", zone)
		return nil
	}
	if c.dryRun {
		log.Info("dry run: would label node", "zone", zone, "current", current)
		return nil
	}

	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"labels": map[string]string{ZoneLabel: zone}},
	})
	if err != nil {
		return fmt.Errorf("marshal patch: %w", err)
	}
	if _, err := c.nodes.Patch(ctx, node.Name, types.MergePatchType, patch,
		metav1.PatchOptions{FieldManager: fieldManager}); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c.m.errors.WithLabelValues("patch").Inc()
		log.Error("failed to patch node, retrying with backoff", "zone", zone, "error", err)
		return err
	}
	c.m.labeled.Inc()
	log.Info("labeled node", "zone", zone, "previous", current)
	return nil
}

func (c *Controller) isExcluded(node *metav1.PartialObjectMetadata) bool {
	for label := range c.excluded {
		if _, ok := node.Labels[label]; ok {
			return true
		}
	}
	return false
}

func (c *Controller) countWithoutZone() int {
	n := 0
	for _, obj := range c.informer.GetStore().List() {
		node, ok := obj.(*metav1.PartialObjectMetadata)
		if !ok || c.isExcluded(node) {
			continue
		}
		if _, ok := node.Labels[ZoneLabel]; !ok {
			n++
		}
	}
	return n
}
