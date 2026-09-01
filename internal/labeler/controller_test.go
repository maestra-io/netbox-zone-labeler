package labeler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	metadatafake "k8s.io/client-go/metadata/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/util/workqueue"

	"github.com/maestra-io/netbox-zone-labeler/internal/netbox"
)

// testScheme registers PartialObjectMetadata as the Go type behind v1/Node so
// the fake tracker files the objects under the nodes resource.
func testScheme() *runtime.Scheme {
	s := metadatafake.NewTestScheme()
	s.AddKnownTypeWithName(schema.GroupVersionKind{Version: "v1", Kind: "Node"}, &metav1.PartialObjectMetadata{})
	return s
}

func node(name string, labels map[string]string) *metav1.PartialObjectMetadata {
	return &metav1.PartialObjectMetadata{
		TypeMeta:   metav1.TypeMeta{Kind: "Node", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
	}
}

// fakeLookup answers LookupRack from a table and can fail the first N calls
// for a host with a transient error.
type fakeLookup struct {
	mu        sync.Mutex
	racks     map[string]string
	errs      map[string]error
	failFirst map[string]int
	calls     map[string]int
}

func newFakeLookup() *fakeLookup {
	return &fakeLookup{racks: map[string]string{}, errs: map[string]error{}, failFirst: map[string]int{}, calls: map[string]int{}}
}

func (f *fakeLookup) LookupRack(_ context.Context, host string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[host]++
	if n := f.failFirst[host]; n > 0 {
		f.failFirst[host] = n - 1
		return "", errors.New("netbox 502")
	}
	if err, ok := f.errs[host]; ok {
		return "", err
	}
	rack, ok := f.racks[host]
	if !ok {
		return "", fmt.Errorf("device %q: %w", host, netbox.ErrNoZone)
	}
	return rack, nil
}

func (f *fakeLookup) callsFor(host string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[host]
}

type harness struct {
	t      *testing.T
	ctrl   *Controller
	client *metadatafake.FakeMetadataClient
	lookup *fakeLookup
	ctx    context.Context
}

// harnessCfg tweaks the controller options or the fake client before Run
// starts; reactors must be installed before the informer's first List.
type harnessCfg struct {
	opts   func(*Options)
	client func(*metadatafake.FakeMetadataClient)
}

func newHarness(t *testing.T, lookup *fakeLookup, cfg *harnessCfg, nodes ...runtime.Object) *harness {
	t.Helper()
	if cfg == nil {
		cfg = &harnessCfg{}
	}
	client := metadatafake.NewSimpleMetadataClient(testScheme(), nodes...)
	if cfg.client != nil {
		cfg.client(client)
	}
	o := Options{
		Client:      client,
		Lookup:      lookup,
		Period:      time.Hour,
		Registerer:  prometheus.NewRegistry(),
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		RateLimiter: workqueue.NewTypedItemExponentialFailureRateLimiter[string](time.Millisecond, 20*time.Millisecond),
	}
	if cfg.opts != nil {
		cfg.opts(&o)
	}
	ctrl, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ctrl.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run returned %v", err)
		}
	})
	h := &harness{t: t, ctrl: ctrl, client: client, lookup: lookup, ctx: ctx}
	h.eventually("informer synced", ctrl.Ready)
	return h
}

func (h *harness) eventually(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for %s", what)
}

// settled waits until the queue is drained and nothing is in flight.
func (h *harness) settled() {
	h.t.Helper()
	h.eventually("queue drained", func() bool { return h.ctrl.queue.Len() == 0 })
	time.Sleep(50 * time.Millisecond) // let the in-flight item finish
	h.eventually("queue drained", func() bool { return h.ctrl.queue.Len() == 0 })
}

func (h *harness) zoneOf(name string) string {
	h.t.Helper()
	m, err := h.client.Resource(nodesGVR).Get(h.ctx, name, metav1.GetOptions{})
	if err != nil {
		h.t.Fatal(err)
	}
	return m.Labels[ZoneLabel]
}

func (h *harness) patches() int {
	n := 0
	for _, a := range h.client.Actions() {
		if a.GetVerb() == "patch" {
			n++
		}
	}
	return n
}

func (h *harness) errorsTotal(reason string) float64 {
	return testutil.ToFloat64(h.ctrl.m.errors.WithLabelValues(reason))
}

func TestController_LabelsNode(t *testing.T) {
	lookup := newFakeLookup()
	lookup.racks["node-1"] = "Rack 42"
	h := newHarness(t, lookup, nil, node("node-1", nil))

	h.eventually("label applied", func() bool { return h.zoneOf("node-1") == "rack-42" })
	h.settled()
	if got := h.patches(); got != 1 {
		t.Errorf("patches = %d, want 1", got)
	}
	if got := testutil.ToFloat64(h.ctrl.m.labeled); got != 1 {
		t.Errorf("nodes_labeled_total = %v, want 1", got)
	}
	if got := testutil.ToFloat64(h.ctrl.m.withoutZone); got != 0 {
		t.Errorf("nodes_without_zone = %v, want 0", got)
	}

	// The watch event produced by our own patch must not trigger a second
	// patch, and a full pass on an already-correct node must not either.
	h.ctrl.fullPass()
	h.settled()
	if got := h.patches(); got != 1 {
		t.Errorf("patches after full pass = %d, want 1", got)
	}
	// 3 lookups: initial, the re-check triggered by the watch event of our
	// own patch (labels changed), and the full pass.
	if got := lookup.callsFor("node-1"); got != 3 {
		t.Errorf("lookups = %d, want 3", got)
	}
}

func TestController_CorrectsDrift(t *testing.T) {
	lookup := newFakeLookup()
	lookup.racks["node-1"] = "B2"
	h := newHarness(t, lookup, nil, node("node-1", map[string]string{ZoneLabel: "b1"}))

	h.eventually("label corrected", func() bool { return h.zoneOf("node-1") == "b2" })
}

func TestController_AlreadyCorrectIsNotPatched(t *testing.T) {
	lookup := newFakeLookup()
	lookup.racks["node-1"] = "L130-B14"
	h := newHarness(t, lookup, nil, node("node-1", map[string]string{ZoneLabel: "l130-b14"}))

	h.eventually("looked up", func() bool { return lookup.callsFor("node-1") == 1 })
	h.settled()
	if got := h.patches(); got != 0 {
		t.Errorf("patches = %d, want 0", got)
	}
}

func TestController_ExcludedRoleIsSkipped(t *testing.T) {
	lookup := newFakeLookup()
	lookup.racks["master-1"] = "A1"
	lookup.racks["worker-1"] = "A1"
	h := newHarness(t, lookup, &harnessCfg{opts: func(o *Options) { o.ExcludeRoles = []string{"master", "control-plane"} }},
		node("master-1", map[string]string{"node-role.kubernetes.io/master": ""}),
		node("worker-1", map[string]string{"node-role.kubernetes.io/worker": ""}),
	)

	h.eventually("worker labeled", func() bool { return h.zoneOf("worker-1") == "a1" })
	h.settled()
	if got := lookup.callsFor("master-1"); got != 0 {
		t.Errorf("excluded node was looked up %d times", got)
	}
	if h.zoneOf("master-1") != "" {
		t.Error("excluded node was labeled")
	}
	if got := testutil.ToFloat64(h.ctrl.m.withoutZone); got != 0 {
		t.Errorf("nodes_without_zone = %v, want 0 (excluded nodes do not count)", got)
	}
}

func TestController_MissIsNotAnErrorAndWaitsForFullPass(t *testing.T) {
	lookup := newFakeLookup() // knows nothing about node-1
	h := newHarness(t, lookup, nil, node("node-1", nil))

	h.eventually("looked up", func() bool { return lookup.callsFor("node-1") == 1 })
	h.settled()
	if got := h.patches(); got != 0 {
		t.Errorf("patches = %d, want 0", got)
	}
	for _, r := range []string{"netbox", "patch", "invalid_label", "ambiguous"} {
		if got := h.errorsTotal(r); got != 0 {
			t.Errorf("errors_total{%s} = %v, want 0: a miss is not an error", r, got)
		}
	}
	if got := testutil.ToFloat64(h.ctrl.m.withoutZone); got != 1 {
		t.Errorf("nodes_without_zone = %v, want 1", got)
	}
	// No rate-limited retry: the count stays at 1 until the next full pass.
	time.Sleep(100 * time.Millisecond)
	if got := lookup.callsFor("node-1"); got != 1 {
		t.Errorf("lookups = %d, want 1 (a miss must not be retried with backoff)", got)
	}

	// The node appears in NetBox; the next full pass picks it up.
	lookup.mu.Lock()
	lookup.racks["node-1"] = "C3"
	lookup.mu.Unlock()
	h.ctrl.fullPass()
	h.eventually("labeled after full pass", func() bool { return h.zoneOf("node-1") == "c3" })
}

func TestController_TransientLookupErrorIsRetried(t *testing.T) {
	lookup := newFakeLookup()
	lookup.racks["node-1"] = "D4"
	lookup.failFirst["node-1"] = 2
	h := newHarness(t, lookup, nil, node("node-1", nil))

	h.eventually("labeled after retries", func() bool { return h.zoneOf("node-1") == "d4" })
	h.settled()
	// 2 failures, the success, and the re-check from our own patch's event.
	if got := lookup.callsFor("node-1"); got != 4 {
		t.Errorf("lookups = %d, want 4", got)
	}
	if got := h.errorsTotal("netbox"); got != 2 {
		t.Errorf("errors_total{netbox} = %v, want 2", got)
	}
}

func TestController_AmbiguousIsReportedNotRetried(t *testing.T) {
	lookup := newFakeLookup()
	lookup.errs["node-1"] = fmt.Errorf("device: %w: 2 matches", netbox.ErrAmbiguous)
	h := newHarness(t, lookup, nil, node("node-1", nil))

	h.eventually("reported", func() bool { return h.errorsTotal("ambiguous") == 1 })
	h.settled()
	time.Sleep(100 * time.Millisecond)
	if got := lookup.callsFor("node-1"); got != 1 {
		t.Errorf("lookups = %d, want 1", got)
	}
	if got := h.patches(); got != 0 {
		t.Errorf("patches = %d, want 0", got)
	}
}

func TestController_InvalidRackNameIsReportedNotPatched(t *testing.T) {
	lookup := newFakeLookup()
	lookup.racks["node-1"] = "Rack #3"
	h := newHarness(t, lookup, nil, node("node-1", nil))

	h.eventually("reported", func() bool { return h.errorsTotal("invalid_label") == 1 })
	h.settled()
	if got := h.patches(); got != 0 {
		t.Errorf("patches = %d, want 0", got)
	}
}

func TestController_PatchFailureIsRetried(t *testing.T) {
	lookup := newFakeLookup()
	lookup.racks["node-1"] = "E5"
	var failed bool
	var mu sync.Mutex
	failOnce := func(c *metadatafake.FakeMetadataClient) {
		c.PrependReactor("patch", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
			mu.Lock()
			defer mu.Unlock()
			if failed {
				return false, nil, nil
			}
			failed = true
			return true, nil, errors.New("apiserver 500")
		})
	}
	h := newHarness(t, lookup, &harnessCfg{client: failOnce}, node("node-1", nil))

	h.eventually("labeled after patch retry", func() bool { return h.zoneOf("node-1") == "e5" })
	if got := h.errorsTotal("patch"); got != 1 {
		t.Errorf("errors_total{patch} = %v, want 1", got)
	}
}

func TestController_DryRunPatchesNothing(t *testing.T) {
	lookup := newFakeLookup()
	lookup.racks["node-1"] = "F6"
	h := newHarness(t, lookup, &harnessCfg{opts: func(o *Options) { o.DryRun = true }}, node("node-1", nil))

	h.eventually("looked up", func() bool { return lookup.callsFor("node-1") == 1 })
	h.settled()
	if got := h.patches(); got != 0 {
		t.Errorf("patches = %d, want 0 in dry run", got)
	}
	if h.zoneOf("node-1") != "" {
		t.Error("node was labeled in dry run")
	}
}

func TestController_NewNodeIsLabeled(t *testing.T) {
	lookup := newFakeLookup()
	lookup.racks["node-1"] = "X"
	lookup.racks["node-2"] = "G7"
	h := newHarness(t, lookup, nil, node("node-1", map[string]string{ZoneLabel: "x"}))
	h.settled()

	// A node joins the cluster after start-up: the watch event labels it.
	if err := h.client.Tracker().Add(node("node-2", nil)); err != nil {
		t.Fatal(err)
	}
	h.eventually("new node labeled", func() bool { return h.zoneOf("node-2") == "g7" })
}

func TestController_UpdateEnqueuesOnlyOnLabelChange(t *testing.T) {
	ctrl, err := New(Options{
		Client:     metadatafake.NewSimpleMetadataClient(testScheme()),
		Lookup:     newFakeLookup(),
		Period:     time.Hour,
		Registerer: prometheus.NewRegistry(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ctrl.queue.ShutDown()

	// Kubelet heartbeat: same labels, new resourceVersion.
	old := node("node-1", map[string]string{"a": "1"})
	heartbeat := node("node-1", map[string]string{"a": "1"})
	heartbeat.ResourceVersion = "2"
	ctrl.onUpdate(old, heartbeat)
	if got := ctrl.queue.Len(); got != 0 {
		t.Errorf("queue = %d after heartbeat, want 0", got)
	}

	relabeled := node("node-1", map[string]string{"a": "1", ZoneLabel: "moved"})
	ctrl.onUpdate(old, relabeled)
	if got := ctrl.queue.Len(); got != 1 {
		t.Errorf("queue = %d after label change, want 1", got)
	}

	ctrl.onUpdate("not a node", relabeled)
	if got := ctrl.queue.Len(); got != 1 {
		t.Errorf("queue = %d after garbage event, want 1", got)
	}
}

func TestController_Healthy(t *testing.T) {
	ctrl, err := New(Options{
		Client:     metadatafake.NewSimpleMetadataClient(testScheme()),
		Lookup:     newFakeLookup(),
		Period:     time.Minute,
		Registerer: prometheus.NewRegistry(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ctrl.queue.ShutDown()

	if !ctrl.Healthy() {
		t.Error("not healthy before the first pass")
	}
	ctrl.markPass()
	if !ctrl.Healthy() {
		t.Error("not healthy right after a pass")
	}
	ctrl.lastPass.Store(time.Now().Add(-4 * time.Minute).Unix())
	if ctrl.Healthy() {
		t.Error("healthy with the last pass three periods ago")
	}
}

func TestNew_Validation(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Error("expected error without client and lookup")
	}
	if _, err := New(Options{Client: metadatafake.NewSimpleMetadataClient(testScheme()), Lookup: newFakeLookup()}); err == nil {
		t.Error("expected error without period")
	}
}
