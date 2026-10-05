package gateway

import (
	"context"
	"strings"
	"testing"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	envoyproxytypes "github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"k8s.io/client-go/util/workqueue"
)

func newTestXDSController() *Controller {
	return &Controller{
		xdscache: cachev3.NewSnapshotCache(false, cachev3.IDHash{}, nil),
		xdsState: make(map[string]*xdsNodeState),
		gatewayqueue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "gateway-test"},
		),
	}
}

func testResources() map[resourcev3.Type][]envoyproxytypes.Resource {
	return map[resourcev3.Type][]envoyproxytypes.Resource{
		resourcev3.ListenerType: {&listenerv3.Listener{Name: "listener-80"}},
		resourcev3.RouteType:    {&routev3.RouteConfiguration{Name: "route-80"}},
		resourcev3.ClusterType:  {&clusterv3.Cluster{Name: "cluster-a"}},
	}
}

// drainQueue returns all keys currently in the gateway workqueue.
func drainQueue(c *Controller) []string {
	var keys []string
	for c.gatewayqueue.Len() > 0 {
		key, _ := c.gatewayqueue.Get()
		keys = append(keys, key)
		c.gatewayqueue.Done(key)
	}
	return keys
}

func Test_computeResourcesHash(t *testing.T) {
	a := computeResourcesHash(testResources())
	b := computeResourcesHash(testResources())
	if a != b {
		t.Errorf("hash is not stable for identical resources: %q vs %q", a, b)
	}

	changed := testResources()
	changed[resourcev3.ClusterType] = []envoyproxytypes.Resource{&clusterv3.Cluster{Name: "cluster-b"}}
	if computeResourcesHash(changed) == a {
		t.Errorf("hash did not change when resources changed")
	}

	empty := computeResourcesHash(map[resourcev3.Type][]envoyproxytypes.Resource{})
	if empty == a {
		t.Errorf("hash of empty resources equals hash of non-empty resources")
	}
}

func Test_requiredACKTypes(t *testing.T) {
	withListeners := requiredACKTypes(testResources())
	if !withListeners.Has(resourcev3.RouteType) {
		t.Errorf("RDS ACK should be required when the snapshot has listeners and routes")
	}
	if !withListeners.Has(resourcev3.ClusterType) || !withListeners.Has(resourcev3.ListenerType) {
		t.Errorf("CDS and LDS ACKs should always be required")
	}

	noListeners := testResources()
	noListeners[resourcev3.ListenerType] = nil
	if requiredACKTypes(noListeners).Has(resourcev3.RouteType) {
		t.Errorf("RDS ACK should not be required without listeners: Envoy never subscribes")
	}
}

func Test_applyXDSConfig_waitsForFullACK(t *testing.T) {
	c := newTestXDSController()
	ctx := context.Background()
	resources := testResources()
	version := computeResourcesHash(resources)

	// The first sync pushes the snapshot and must report pending.
	err, pending := c.applyXDSConfig(ctx, "node-1", "default/gw", resources)
	if err != nil || !pending {
		t.Fatalf("first apply should be pending without error, got err=%v pending=%v", err, pending)
	}

	// A partial ACK (only CDS and LDS) is still pending; the bad-regex NACK
	// arrives on RDS after the other types have ACKed, so a partial ACK must
	// not flip the gateway to Programmed=True.
	c.handleXDSRequest("node-1", resourcev3.ClusterType, version, "")
	c.handleXDSRequest("node-1", resourcev3.ListenerType, version, "")
	if keys := drainQueue(c); len(keys) != 0 {
		t.Errorf("partial ACK should not requeue the gateway, got %v", keys)
	}
	err, pending = c.applyXDSConfig(ctx, "node-1", "default/gw", resources)
	if err != nil || !pending {
		t.Fatalf("partially ACKed apply should be pending without error, got err=%v pending=%v", err, pending)
	}

	// The final ACK completes the set and requeues the gateway.
	c.handleXDSRequest("node-1", resourcev3.RouteType, version, "")
	if keys := drainQueue(c); len(keys) != 1 || keys[0] != "default/gw" {
		t.Errorf("full ACK should requeue the gateway once, got %v", keys)
	}
	err, pending = c.applyXDSConfig(ctx, "node-1", "default/gw", resources)
	if err != nil || pending {
		t.Fatalf("fully ACKed apply should be programmed, got err=%v pending=%v", err, pending)
	}
}

func Test_applyXDSConfig_surfacesNACK(t *testing.T) {
	c := newTestXDSController()
	ctx := context.Background()
	resources := testResources()
	version := computeResourcesHash(resources)

	if err, _ := c.applyXDSConfig(ctx, "node-1", "default/gw", resources); err != nil {
		t.Fatalf("unexpected apply error: %v", err)
	}

	c.handleXDSRequest("node-1", resourcev3.ClusterType, version, "")
	c.handleXDSRequest("node-1", resourcev3.ListenerType, version, "")
	c.handleXDSRequest("node-1", resourcev3.RouteType, "", "bad regex")
	if keys := drainQueue(c); len(keys) != 1 || keys[0] != "default/gw" {
		t.Errorf("NACK should requeue the gateway once, got %v", keys)
	}

	err, pending := c.applyXDSConfig(ctx, "node-1", "default/gw", resources)
	if err == nil || !strings.Contains(err.Error(), "bad regex") {
		t.Fatalf("NACKed apply should surface Envoy's error, got err=%v", err)
	}
	if pending {
		t.Errorf("NACKed apply should not be pending")
	}

	// Envoy resends the same NACK on every re-push of the same version (sotw
	// behaviour); identical NACKs must not requeue again or the controller
	// would hot-loop.
	c.handleXDSRequest("node-1", resourcev3.RouteType, "", "bad regex")
	if keys := drainQueue(c); len(keys) != 0 {
		t.Errorf("repeated identical NACK should not requeue the gateway, got %v", keys)
	}

	// Fixing the config (new content hash) clears the NACK and goes back to
	// pending until Envoy confirms the new version.
	fixed := testResources()
	fixed[resourcev3.RouteType] = []envoyproxytypes.Resource{&routev3.RouteConfiguration{Name: "route-81"}}
	err, pending = c.applyXDSConfig(ctx, "node-1", "default/gw", fixed)
	if err != nil || !pending {
		t.Fatalf("apply of changed config should clear the NACK and be pending, got err=%v pending=%v", err, pending)
	}
}

func Test_applyXDSConfig_staleNACKClearedByFullACK(t *testing.T) {
	c := newTestXDSController()
	ctx := context.Background()
	resources := testResources()
	version := computeResourcesHash(resources)

	if err, _ := c.applyXDSConfig(ctx, "node-1", "default/gw", resources); err != nil {
		t.Fatalf("unexpected apply error: %v", err)
	}

	// A NACK for a previous config may race with a new push. Once every
	// required type ACKs the current version the stale NACK must be dropped.
	c.handleXDSRequest("node-1", resourcev3.RouteType, "", "stale error for the old config")
	c.handleXDSRequest("node-1", resourcev3.ClusterType, version, "")
	c.handleXDSRequest("node-1", resourcev3.ListenerType, version, "")
	c.handleXDSRequest("node-1", resourcev3.RouteType, version, "")

	err, pending := c.applyXDSConfig(ctx, "node-1", "default/gw", resources)
	if err != nil || pending {
		t.Fatalf("full ACK should clear a stale NACK, got err=%v pending=%v", err, pending)
	}
}

func Test_applyXDSConfig_ackBeforeFirstSync(t *testing.T) {
	// After a controller restart Envoy reconnects already holding the current
	// configuration and reports its version before any gateway sync has
	// registered state.
	c := newTestXDSController()
	ctx := context.Background()
	resources := testResources()
	version := computeResourcesHash(resources)

	c.handleXDSRequest("node-1", resourcev3.ClusterType, version, "")
	c.handleXDSRequest("node-1", resourcev3.ListenerType, version, "")
	c.handleXDSRequest("node-1", resourcev3.RouteType, version, "")
	if keys := drainQueue(c); len(keys) != 0 {
		t.Errorf("requests for an unregistered node should not requeue anything, got %v", keys)
	}

	err, pending := c.applyXDSConfig(ctx, "node-1", "default/gw", resources)
	if err != nil || pending {
		t.Fatalf("apply after restart with matching ACKs should be programmed immediately, got err=%v pending=%v", err, pending)
	}
}

func Test_forgetXDSState(t *testing.T) {
	c := newTestXDSController()
	ctx := context.Background()
	resources := testResources()
	version := computeResourcesHash(resources)

	if err, _ := c.applyXDSConfig(ctx, "node-1", "default/gw", resources); err != nil {
		t.Fatalf("unexpected apply error: %v", err)
	}
	c.forgetXDSState("node-1")

	// Requests for a forgotten node must not requeue the deleted gateway.
	c.handleXDSRequest("node-1", resourcev3.RouteType, version, "")
	if keys := drainQueue(c); len(keys) != 0 {
		t.Errorf("requests after forgetXDSState should not requeue, got %v", keys)
	}
}
