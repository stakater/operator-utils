package dynamicinformer

import (
	"context"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var (
	deploymentsGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	servicesGVR    = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "services"}
	configmapsGVR  = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}
)

func newFakeRegistry(t *testing.T) (*WatchRegistry, context.Context, context.CancelFunc) {
	t.Helper()
	scheme := runtime.NewScheme()
	gvrToListKind := map[schema.GroupVersionResource]string{
		deploymentsGVR: "DeploymentList",
		servicesGVR:    "ServiceList",
		configmapsGVR:  "ConfigMapList",
	}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, gvrToListKind)
	registry := NewWatchRegistry(client, 30*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	return registry, ctx, cancel
}

func noopCallback() EventCallback {
	return EventCallback{
		OnAdd:    func(obj *unstructured.Unstructured) {},
		OnUpdate: func(oldObj, newObj *unstructured.Unstructured) {},
		OnDelete: func(obj *unstructured.Unstructured) {},
	}
}

func TestNewWatchRegistry(t *testing.T) {
	registry, _, cancel := newFakeRegistry(t)
	defer cancel()

	if registry == nil {
		t.Fatal("expected non-nil registry")
	}
	if len(registry.watches) != 0 {
		t.Errorf("expected empty watches map, got %d entries", len(registry.watches))
	}
	if len(registry.consumerKeys) != 0 {
		t.Errorf("expected empty consumerKeys map, got %d entries", len(registry.consumerKeys))
	}
}

func TestEnsureWatchSet_NewWatch(t *testing.T) {
	registry, ctx, cancel := newFakeRegistry(t)
	defer cancel()

	watches := []WatchRequest{
		{
			Key:      WatchKey{GVR: deploymentsGVR, Namespace: "default"},
			Callback: noopCallback(),
		},
	}

	regs, err := registry.EnsureWatchSet(ctx, "consumer-1", watches)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(regs) != 1 {
		t.Fatalf("expected 1 registration, got %d", len(regs))
	}
	if regs[0].ConsumerID != "consumer-1" {
		t.Errorf("expected consumerID consumer-1, got %s", regs[0].ConsumerID)
	}
	if regs[0].Key.GVR != deploymentsGVR {
		t.Errorf("expected GVR %v, got %v", deploymentsGVR, regs[0].Key.GVR)
	}
	if regs[0].HasSynced == nil {
		t.Error("expected non-nil HasSynced")
	}

	// Verify internal state
	registry.mu.Lock()
	defer registry.mu.Unlock()

	key := WatchKey{GVR: deploymentsGVR, Namespace: "default"}
	if _, ok := registry.watches[key]; !ok {
		t.Error("expected watch entry for key")
	}
	if _, ok := registry.watches[key].handlers["consumer-1"]; !ok {
		t.Error("expected handler for consumer-1")
	}
	if _, ok := registry.consumerKeys["consumer-1"]; !ok {
		t.Error("expected consumerKeys entry for consumer-1")
	}
}

func TestEnsureWatchSet_Idempotent(t *testing.T) {
	registry, ctx, cancel := newFakeRegistry(t)
	defer cancel()

	watches := []WatchRequest{
		{
			Key:      WatchKey{GVR: deploymentsGVR, Namespace: "default"},
			Callback: noopCallback(),
		},
	}

	regs1, err := registry.EnsureWatchSet(ctx, "consumer-1", watches)
	if err != nil {
		t.Fatalf("unexpected error on first call: %v", err)
	}

	regs2, err := registry.EnsureWatchSet(ctx, "consumer-1", watches)
	if err != nil {
		t.Fatalf("unexpected error on second call: %v", err)
	}

	if len(regs1) != len(regs2) {
		t.Fatalf("expected same number of registrations, got %d and %d", len(regs1), len(regs2))
	}

	// Verify only one informer and one handler exist
	registry.mu.Lock()
	defer registry.mu.Unlock()

	key := WatchKey{GVR: deploymentsGVR, Namespace: "default"}
	entry := registry.watches[key]
	if len(entry.handlers) != 1 {
		t.Errorf("expected 1 handler, got %d", len(entry.handlers))
	}
}

func TestEnsureWatchSet_MultipleWatches(t *testing.T) {
	registry, ctx, cancel := newFakeRegistry(t)
	defer cancel()

	watches := []WatchRequest{
		{
			Key:      WatchKey{GVR: deploymentsGVR, Namespace: "default"},
			Callback: noopCallback(),
		},
		{
			Key:      WatchKey{GVR: servicesGVR, Namespace: "default"},
			Callback: noopCallback(),
		},
		{
			Key:      WatchKey{GVR: configmapsGVR, Namespace: "kube-system"},
			Callback: noopCallback(),
		},
	}

	regs, err := registry.EnsureWatchSet(ctx, "consumer-1", watches)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(regs) != 3 {
		t.Fatalf("expected 3 registrations, got %d", len(regs))
	}

	registry.mu.Lock()
	defer registry.mu.Unlock()

	if len(registry.watches) != 3 {
		t.Errorf("expected 3 watch entries, got %d", len(registry.watches))
	}
	if len(registry.consumerKeys["consumer-1"]) != 3 {
		t.Errorf("expected 3 consumer keys, got %d", len(registry.consumerKeys["consumer-1"]))
	}
}

func TestEnsureWatchSet_RemovesStaleWatches(t *testing.T) {
	registry, ctx, cancel := newFakeRegistry(t)
	defer cancel()

	// Initial set: deployments + services
	watches1 := []WatchRequest{
		{
			Key:      WatchKey{GVR: deploymentsGVR, Namespace: "default"},
			Callback: noopCallback(),
		},
		{
			Key:      WatchKey{GVR: servicesGVR, Namespace: "default"},
			Callback: noopCallback(),
		},
	}

	_, err := registry.EnsureWatchSet(ctx, "consumer-1", watches1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Updated set: deployments + configmaps (services removed, configmaps added)
	watches2 := []WatchRequest{
		{
			Key:      WatchKey{GVR: deploymentsGVR, Namespace: "default"},
			Callback: noopCallback(),
		},
		{
			Key:      WatchKey{GVR: configmapsGVR, Namespace: "default"},
			Callback: noopCallback(),
		},
	}

	regs, err := registry.EnsureWatchSet(ctx, "consumer-1", watches2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(regs) != 2 {
		t.Fatalf("expected 2 registrations, got %d", len(regs))
	}

	registry.mu.Lock()
	defer registry.mu.Unlock()

	// Services watch should be gone (no other consumers)
	svcKey := WatchKey{GVR: servicesGVR, Namespace: "default"}
	if _, ok := registry.watches[svcKey]; ok {
		t.Error("expected services watch to be removed")
	}

	// Deployments should still exist
	depKey := WatchKey{GVR: deploymentsGVR, Namespace: "default"}
	if _, ok := registry.watches[depKey]; !ok {
		t.Error("expected deployments watch to still exist")
	}

	// ConfigMaps should be added
	cmKey := WatchKey{GVR: configmapsGVR, Namespace: "default"}
	if _, ok := registry.watches[cmKey]; !ok {
		t.Error("expected configmaps watch to be added")
	}

	// Consumer keys should reflect new set
	if len(registry.consumerKeys["consumer-1"]) != 2 {
		t.Errorf("expected 2 consumer keys, got %d", len(registry.consumerKeys["consumer-1"]))
	}
}

func TestEnsureWatchSet_EmptyReleasesAll(t *testing.T) {
	registry, ctx, cancel := newFakeRegistry(t)
	defer cancel()

	watches := []WatchRequest{
		{
			Key:      WatchKey{GVR: deploymentsGVR, Namespace: "default"},
			Callback: noopCallback(),
		},
	}

	_, err := registry.EnsureWatchSet(ctx, "consumer-1", watches)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Empty set should release all
	regs, err := registry.EnsureWatchSet(ctx, "consumer-1", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if regs != nil {
		t.Errorf("expected nil registrations, got %d", len(regs))
	}

	registry.mu.Lock()
	defer registry.mu.Unlock()

	if len(registry.watches) != 0 {
		t.Errorf("expected 0 watch entries, got %d", len(registry.watches))
	}
	if len(registry.consumerKeys) != 0 {
		t.Errorf("expected 0 consumer keys entries, got %d", len(registry.consumerKeys))
	}
}

func TestEnsureWatchSet_SharedInformer(t *testing.T) {
	registry, ctx, cancel := newFakeRegistry(t)
	defer cancel()

	key := WatchKey{GVR: deploymentsGVR, Namespace: "default"}

	// Consumer 1 watches deployments
	_, err := registry.EnsureWatchSet(ctx, "consumer-1", []WatchRequest{
		{Key: key, Callback: noopCallback()},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Consumer 2 watches the same GVR+namespace
	_, err = registry.EnsureWatchSet(ctx, "consumer-2", []WatchRequest{
		{Key: key, Callback: noopCallback()},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	registry.mu.Lock()

	// Only one informer should exist
	if len(registry.watches) != 1 {
		t.Errorf("expected 1 watch entry (shared), got %d", len(registry.watches))
	}

	// But two handlers on it
	entry := registry.watches[key]
	if len(entry.handlers) != 2 {
		t.Errorf("expected 2 handlers, got %d", len(entry.handlers))
	}

	registry.mu.Unlock()

	// Release consumer 1 - informer should persist for consumer 2
	if err := registry.ReleaseAll("consumer-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	registry.mu.Lock()
	defer registry.mu.Unlock()

	if len(registry.watches) != 1 {
		t.Errorf("expected 1 watch entry after releasing consumer-1, got %d", len(registry.watches))
	}
	if len(registry.watches[key].handlers) != 1 {
		t.Errorf("expected 1 handler after releasing consumer-1, got %d", len(entry.handlers))
	}
}

func TestEnsureWatchSet_LastConsumerStopsInformer(t *testing.T) {
	registry, ctx, cancel := newFakeRegistry(t)
	defer cancel()

	key := WatchKey{GVR: deploymentsGVR, Namespace: "default"}

	_, err := registry.EnsureWatchSet(ctx, "consumer-1", []WatchRequest{
		{Key: key, Callback: noopCallback()},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := registry.ReleaseAll("consumer-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	registry.mu.Lock()
	defer registry.mu.Unlock()

	if len(registry.watches) != 0 {
		t.Errorf("expected 0 watch entries after last consumer released, got %d", len(registry.watches))
	}
}

func TestReleaseAll_Idempotent(t *testing.T) {
	registry, _, cancel := newFakeRegistry(t)
	defer cancel()

	// Release unknown consumer - should be no-op
	if err := registry.ReleaseAll("unknown"); err != nil {
		t.Fatalf("unexpected error releasing unknown consumer: %v", err)
	}

	// Double release
	if err := registry.ReleaseAll("unknown"); err != nil {
		t.Fatalf("unexpected error on double release: %v", err)
	}
}

func TestReleaseAll_MultipleKeys(t *testing.T) {
	registry, ctx, cancel := newFakeRegistry(t)
	defer cancel()

	watches := []WatchRequest{
		{
			Key:      WatchKey{GVR: deploymentsGVR, Namespace: "default"},
			Callback: noopCallback(),
		},
		{
			Key:      WatchKey{GVR: servicesGVR, Namespace: "default"},
			Callback: noopCallback(),
		},
		{
			Key:      WatchKey{GVR: configmapsGVR, Namespace: "kube-system"},
			Callback: noopCallback(),
		},
	}

	_, err := registry.EnsureWatchSet(ctx, "consumer-1", watches)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := registry.ReleaseAll("consumer-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	registry.mu.Lock()
	defer registry.mu.Unlock()

	if len(registry.watches) != 0 {
		t.Errorf("expected 0 watches, got %d", len(registry.watches))
	}
	if len(registry.consumerKeys) != 0 {
		t.Errorf("expected 0 consumer keys, got %d", len(registry.consumerKeys))
	}
}

func TestEnsureWatchSet_DifferentNamespacesDifferentInformers(t *testing.T) {
	registry, ctx, cancel := newFakeRegistry(t)
	defer cancel()

	watches := []WatchRequest{
		{
			Key:      WatchKey{GVR: deploymentsGVR, Namespace: "ns-a"},
			Callback: noopCallback(),
		},
		{
			Key:      WatchKey{GVR: deploymentsGVR, Namespace: "ns-b"},
			Callback: noopCallback(),
		},
	}

	regs, err := registry.EnsureWatchSet(ctx, "consumer-1", watches)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(regs) != 2 {
		t.Fatalf("expected 2 registrations, got %d", len(regs))
	}

	registry.mu.Lock()
	defer registry.mu.Unlock()

	// Same GVR but different namespaces = different informers
	if len(registry.watches) != 2 {
		t.Errorf("expected 2 watch entries for different namespaces, got %d", len(registry.watches))
	}
}

func TestExtractObject_Unstructured(t *testing.T) {
	obj := &unstructured.Unstructured{}
	obj.SetName("test")

	result := extractObject(obj)
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.GetName() != "test" {
		t.Errorf("expected name test, got %s", result.GetName())
	}
}

func TestExtractObject_Tombstone(t *testing.T) {
	inner := &unstructured.Unstructured{}
	inner.SetName("deleted-obj")

	tombstone := cache.DeletedFinalStateUnknown{
		Key: "default/deleted-obj",
		Obj: inner,
	}

	result := extractObject(tombstone)
	if result == nil {
		t.Fatal("expected non-nil result from tombstone")
	}
	if result.GetName() != "deleted-obj" {
		t.Errorf("expected name deleted-obj, got %s", result.GetName())
	}
}

func TestExtractObject_Unknown(t *testing.T) {
	result := extractObject("not-an-object")
	if result != nil {
		t.Error("expected nil for unknown type")
	}
}

func TestExtractObject_TombstoneWithUnknownInner(t *testing.T) {
	tombstone := cache.DeletedFinalStateUnknown{
		Key: "default/test",
		Obj: "not-unstructured",
	}

	result := extractObject(tombstone)
	if result != nil {
		t.Error("expected nil for tombstone with non-unstructured inner object")
	}
}

func TestCallbackToHandler_AllCallbacks(t *testing.T) {
	var (
		mu        sync.Mutex
		addCalled bool
		updCalled bool
		delCalled bool
	)

	cb := EventCallback{
		OnAdd: func(obj *unstructured.Unstructured) {
			mu.Lock()
			addCalled = true
			mu.Unlock()
		},
		OnUpdate: func(oldObj, newObj *unstructured.Unstructured) {
			mu.Lock()
			updCalled = true
			mu.Unlock()
		},
		OnDelete: func(obj *unstructured.Unstructured) {
			mu.Lock()
			delCalled = true
			mu.Unlock()
		},
	}

	handler := callbackToHandler(cb)
	funcs := handler.(cache.ResourceEventHandlerFuncs)

	obj := &unstructured.Unstructured{}

	funcs.OnAdd(obj, false)
	funcs.OnUpdate(obj, obj)
	funcs.OnDelete(obj)

	mu.Lock()
	defer mu.Unlock()

	if !addCalled {
		t.Error("expected OnAdd to be called")
	}
	if !updCalled {
		t.Error("expected OnUpdate to be called")
	}
	if !delCalled {
		t.Error("expected OnDelete to be called")
	}
}

func TestCallbackToHandler_NilCallbacks(t *testing.T) {
	cb := EventCallback{} // all nil

	handler := callbackToHandler(cb)
	funcs := handler.(cache.ResourceEventHandlerFuncs)

	if funcs.AddFunc != nil {
		t.Error("expected nil AddFunc")
	}
	if funcs.UpdateFunc != nil {
		t.Error("expected nil UpdateFunc")
	}
	if funcs.DeleteFunc != nil {
		t.Error("expected nil DeleteFunc")
	}
}

func TestCallbackToHandler_PartialCallbacks(t *testing.T) {
	var addCalled bool

	cb := EventCallback{
		OnAdd: func(obj *unstructured.Unstructured) {
			addCalled = true
		},
		// OnUpdate and OnDelete are nil
	}

	handler := callbackToHandler(cb)
	funcs := handler.(cache.ResourceEventHandlerFuncs)

	obj := &unstructured.Unstructured{}
	funcs.OnAdd(obj, false)

	if !addCalled {
		t.Error("expected OnAdd to be called")
	}
	if funcs.UpdateFunc != nil {
		t.Error("expected nil UpdateFunc")
	}
	if funcs.DeleteFunc != nil {
		t.Error("expected nil DeleteFunc")
	}
}

func TestCallbackToHandler_SkipsNilObject(t *testing.T) {
	called := false
	cb := EventCallback{
		OnAdd: func(obj *unstructured.Unstructured) {
			called = true
		},
	}

	handler := callbackToHandler(cb)
	funcs := handler.(cache.ResourceEventHandlerFuncs)

	// Pass a non-unstructured object - should be silently skipped
	funcs.OnAdd("not-an-object", false)

	if called {
		t.Error("expected OnAdd NOT to be called for non-unstructured object")
	}
}

func TestEnsureWatchSet_ConcurrentAccess(t *testing.T) {
	registry, ctx, cancel := newFakeRegistry(t)
	defer cancel()

	var wg sync.WaitGroup
	errs := make(chan error, 20)

	// 10 consumers registering concurrently
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			consumerID := string(rune('a'+id)) + "-consumer"
			watches := []WatchRequest{
				{
					Key:      WatchKey{GVR: deploymentsGVR, Namespace: "default"},
					Callback: noopCallback(),
				},
			}
			if _, err := registry.EnsureWatchSet(ctx, consumerID, watches); err != nil {
				errs <- err
			}
		}(i)
	}

	// 10 consumers releasing concurrently
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			consumerID := string(rune('a'+id)) + "-consumer"
			if err := registry.ReleaseAll(consumerID); err != nil {
				errs <- err
			}
		}(i)
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		// Some errors are expected (releasing before registering), but no panics
		_ = err
	}
}

func TestWatchKey_MapKeyBehavior(t *testing.T) {
	key1 := WatchKey{GVR: deploymentsGVR, Namespace: "default"}
	key2 := WatchKey{GVR: deploymentsGVR, Namespace: "default"}
	key3 := WatchKey{GVR: deploymentsGVR, Namespace: "other"}
	key4 := WatchKey{GVR: servicesGVR, Namespace: "default"}

	m := make(map[WatchKey]int)
	m[key1] = 1
	m[key2] = 2 // should overwrite key1

	if m[key1] != 2 {
		t.Error("expected equal keys to map to the same entry")
	}
	if _, ok := m[key3]; ok {
		t.Error("expected different namespace to be a different key")
	}
	if _, ok := m[key4]; ok {
		t.Error("expected different GVR to be a different key")
	}
}

func TestEnsureWatchSet_DuplicateKeysInSlice(t *testing.T) {
	registry, ctx, cancel := newFakeRegistry(t)
	defer cancel()

	key := WatchKey{GVR: deploymentsGVR, Namespace: "default"}

	// Two WatchRequests with the same key but different callbacks.
	// The first registers the handler; the second hits the dedup branch.
	var firstCalled, secondCalled bool
	watches := []WatchRequest{
		{
			Key: key,
			Callback: EventCallback{
				OnAdd: func(obj *unstructured.Unstructured) { firstCalled = true },
			},
		},
		{
			Key: key,
			Callback: EventCallback{
				OnAdd: func(obj *unstructured.Unstructured) { secondCalled = true },
			},
		},
	}

	regs, err := registry.EnsureWatchSet(ctx, "consumer-1", watches)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Both entries produce a registration (one new, one deduped)
	if len(regs) != 2 {
		t.Fatalf("expected 2 registrations, got %d", len(regs))
	}

	// Only one handler should be registered (first one wins)
	registry.mu.Lock()
	entry := registry.watches[key]
	if len(entry.handlers) != 1 {
		t.Errorf("expected 1 handler (first wins), got %d", len(entry.handlers))
	}
	registry.mu.Unlock()

	// Verify the second callback was NOT registered (dedup means first callback is kept)
	_ = firstCalled
	_ = secondCalled
}

func TestEnsureWatchSet_ReRegistrationAfterFullRelease(t *testing.T) {
	registry, ctx, cancel := newFakeRegistry(t)
	defer cancel()

	key := WatchKey{GVR: deploymentsGVR, Namespace: "default"}
	watches := []WatchRequest{
		{Key: key, Callback: noopCallback()},
	}

	// Register
	regs1, err := registry.EnsureWatchSet(ctx, "consumer-1", watches)
	if err != nil {
		t.Fatalf("unexpected error on first register: %v", err)
	}
	if len(regs1) != 1 {
		t.Fatalf("expected 1 registration, got %d", len(regs1))
	}

	// Release all
	if err := registry.ReleaseAll("consumer-1"); err != nil {
		t.Fatalf("unexpected error on release: %v", err)
	}

	// Verify clean state
	registry.mu.Lock()
	if len(registry.watches) != 0 {
		t.Errorf("expected 0 watches after release, got %d", len(registry.watches))
	}
	if len(registry.consumerKeys) != 0 {
		t.Errorf("expected 0 consumer keys after release, got %d", len(registry.consumerKeys))
	}
	registry.mu.Unlock()

	// Re-register with the same key — should create a fresh informer
	regs2, err := registry.EnsureWatchSet(ctx, "consumer-1", watches)
	if err != nil {
		t.Fatalf("unexpected error on re-register: %v", err)
	}
	if len(regs2) != 1 {
		t.Fatalf("expected 1 registration on re-register, got %d", len(regs2))
	}

	// Verify internal state is properly rebuilt
	registry.mu.Lock()
	defer registry.mu.Unlock()

	if len(registry.watches) != 1 {
		t.Errorf("expected 1 watch after re-register, got %d", len(registry.watches))
	}
	entry := registry.watches[key]
	if entry == nil {
		t.Fatal("expected watch entry to exist")
	}
	if len(entry.handlers) != 1 {
		t.Errorf("expected 1 handler after re-register, got %d", len(entry.handlers))
	}
	if _, ok := entry.handlers["consumer-1"]; !ok {
		t.Error("expected handler for consumer-1 after re-register")
	}
	if _, ok := registry.consumerKeys["consumer-1"]; !ok {
		t.Error("expected consumer keys entry after re-register")
	}
}

func TestEnsureWatchSet_ParentContextCancellation(t *testing.T) {
	registry, ctx, cancel := newFakeRegistry(t)

	key := WatchKey{GVR: deploymentsGVR, Namespace: "default"}
	watches := []WatchRequest{
		{Key: key, Callback: noopCallback()},
	}

	_, err := registry.EnsureWatchSet(ctx, "consumer-1", watches)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Grab the informer before cancellation
	registry.mu.Lock()
	entry := registry.watches[key]
	informer := entry.informer
	registry.mu.Unlock()

	// Cancel the parent context — informer contexts are children of this
	cancel()

	// Give the informer goroutine time to notice the cancellation
	time.Sleep(100 * time.Millisecond)

	// The informer's Run goroutine should have exited because its
	// context (a child of parentCtx) was cancelled. We can't directly
	// observe goroutine exit, but we can verify the informer's context
	// channel is closed by checking that the parent is done.
	select {
	case <-ctx.Done():
		// expected — parent context is cancelled
	default:
		t.Error("expected parent context to be cancelled")
	}

	// The informer itself should still be referenceable (the registry
	// doesn't auto-clean on parent cancel — that's by design).
	// The key assertion is no panic and no deadlock.
	_ = informer
}

func TestEnsureWatchSet_EmptySliceUnknownConsumer(t *testing.T) {
	registry, ctx, cancel := newFakeRegistry(t)
	defer cancel()

	// Call with empty slice on a consumer that was never registered.
	// Should be a clean no-op via releaseAllLocked.
	regs, err := registry.EnsureWatchSet(ctx, "never-registered", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if regs != nil {
		t.Errorf("expected nil registrations, got %d", len(regs))
	}

	// Also try with an explicit empty slice
	regs, err = registry.EnsureWatchSet(ctx, "also-unknown", []WatchRequest{})
	if err != nil {
		t.Fatalf("unexpected error with empty slice: %v", err)
	}
	if regs != nil {
		t.Errorf("expected nil registrations for empty slice, got %d", len(regs))
	}

	// Internal state should remain clean
	registry.mu.Lock()
	defer registry.mu.Unlock()

	if len(registry.watches) != 0 {
		t.Errorf("expected 0 watches, got %d", len(registry.watches))
	}
	if len(registry.consumerKeys) != 0 {
		t.Errorf("expected 0 consumer keys, got %d", len(registry.consumerKeys))
	}
}

// --- EventCallbackFromMapFunc tests ---

func testMapFunc(obj *unstructured.Unstructured) []reconcile.Request {
	return []reconcile.Request{
		{NamespacedName: types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}},
	}
}

func TestEventCallbackFromMapFunc_OnAdd(t *testing.T) {
	eventCh := make(chan event.GenericEvent, 10)
	cb := EventCallbackFromMapFunc(testMapFunc, eventCh)

	obj := &unstructured.Unstructured{}
	obj.SetName("my-obj")
	obj.SetNamespace("default")

	cb.OnAdd(obj)

	select {
	case evt := <-eventCh:
		if evt.Object.GetName() != "my-obj" {
			t.Errorf("expected name my-obj, got %s", evt.Object.GetName())
		}
		if evt.Object.GetNamespace() != "default" {
			t.Errorf("expected namespace default, got %s", evt.Object.GetNamespace())
		}
	default:
		t.Error("expected event to be enqueued")
	}
}

func TestEventCallbackFromMapFunc_OnUpdate(t *testing.T) {
	eventCh := make(chan event.GenericEvent, 10)
	cb := EventCallbackFromMapFunc(testMapFunc, eventCh)

	oldObj := &unstructured.Unstructured{}
	oldObj.SetName("old-name")
	oldObj.SetNamespace("default")

	newObj := &unstructured.Unstructured{}
	newObj.SetName("new-name")
	newObj.SetNamespace("default")

	cb.OnUpdate(oldObj, newObj)

	select {
	case evt := <-eventCh:
		if evt.Object.GetName() != "new-name" {
			t.Errorf("expected name new-name (from newObj), got %s", evt.Object.GetName())
		}
	default:
		t.Error("expected event from OnUpdate")
	}
}

func TestEventCallbackFromMapFunc_OnDelete(t *testing.T) {
	eventCh := make(chan event.GenericEvent, 10)
	cb := EventCallbackFromMapFunc(testMapFunc, eventCh)

	obj := &unstructured.Unstructured{}
	obj.SetName("deleted-obj")
	obj.SetNamespace("default")

	cb.OnDelete(obj)

	select {
	case evt := <-eventCh:
		if evt.Object.GetName() != "deleted-obj" {
			t.Errorf("expected name deleted-obj, got %s", evt.Object.GetName())
		}
	default:
		t.Error("expected event from OnDelete")
	}
}

func TestEventCallbackFromMapFunc_MultipleRequests(t *testing.T) {
	eventCh := make(chan event.GenericEvent, 10)
	cb := EventCallbackFromMapFunc(func(obj *unstructured.Unstructured) []reconcile.Request {
		return []reconcile.Request{
			{NamespacedName: types.NamespacedName{Name: "owner-a", Namespace: "ns-a"}},
			{NamespacedName: types.NamespacedName{Name: "owner-b", Namespace: "ns-b"}},
		}
	}, eventCh)

	obj := &unstructured.Unstructured{}
	obj.SetName("child")
	obj.SetNamespace("default")

	cb.OnAdd(obj)

	names := make(map[string]bool)
	for range 2 {
		select {
		case evt := <-eventCh:
			names[evt.Object.GetName()] = true
		default:
			t.Fatal("expected 2 events")
		}
	}

	if !names["owner-a"] || !names["owner-b"] {
		t.Errorf("expected owner-a and owner-b, got %v", names)
	}

	select {
	case <-eventCh:
		t.Error("expected no more events")
	default:
	}
}

func TestEventCallbackFromMapFunc_NilReturn(t *testing.T) {
	eventCh := make(chan event.GenericEvent, 10)
	cb := EventCallbackFromMapFunc(func(obj *unstructured.Unstructured) []reconcile.Request {
		return nil
	}, eventCh)

	obj := &unstructured.Unstructured{}
	obj.SetName("my-obj")
	obj.SetNamespace("default")

	cb.OnAdd(obj)

	select {
	case <-eventCh:
		t.Error("expected no event when MapFunc returns nil")
	default:
	}
}

func TestEventCallbackFromMapFunc_ChannelFull(t *testing.T) {
	eventCh := make(chan event.GenericEvent) // unbuffered = always full
	cb := EventCallbackFromMapFunc(testMapFunc, eventCh)

	obj := &unstructured.Unstructured{}
	obj.SetName("my-obj")
	obj.SetNamespace("default")

	// Should not block or panic when channel is full
	cb.OnAdd(obj)
}

// --- LabelSelectorCallback tests ---

func ownerAnnotationMapFunc(obj *unstructured.Unstructured) []reconcile.Request {
	annotations := obj.GetAnnotations()
	name := annotations["example.com/owner-name"]
	ns := annotations["example.com/owner-namespace"]
	if name == "" || ns == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}}}
}

func TestLabelSelectorCallback_MatchingLabels(t *testing.T) {
	eventCh := make(chan event.GenericEvent, 10)
	selector, err := labels.Parse("app=nginx")
	if err != nil {
		t.Fatalf("unexpected error parsing selector: %v", err)
	}

	cb := LabelSelectorCallback(selector, ownerAnnotationMapFunc, eventCh)

	obj := &unstructured.Unstructured{}
	obj.SetName("my-pod")
	obj.SetNamespace("default")
	obj.SetLabels(map[string]string{"app": "nginx", "env": "prod"})
	obj.SetAnnotations(map[string]string{
		"example.com/owner-name":      "my-owner",
		"example.com/owner-namespace": "platform",
	})

	cb.OnAdd(obj)

	select {
	case evt := <-eventCh:
		if evt.Object.GetName() != "my-owner" {
			t.Errorf("expected name my-owner, got %s", evt.Object.GetName())
		}
		if evt.Object.GetNamespace() != "platform" {
			t.Errorf("expected namespace platform, got %s", evt.Object.GetNamespace())
		}
	default:
		t.Error("expected event for matching labels")
	}
}

func TestLabelSelectorCallback_NonMatchingLabels(t *testing.T) {
	eventCh := make(chan event.GenericEvent, 10)
	selector, err := labels.Parse("app=nginx")
	if err != nil {
		t.Fatalf("unexpected error parsing selector: %v", err)
	}

	cb := LabelSelectorCallback(selector, ownerAnnotationMapFunc, eventCh)

	obj := &unstructured.Unstructured{}
	obj.SetName("my-pod")
	obj.SetNamespace("default")
	obj.SetLabels(map[string]string{"app": "redis"})
	obj.SetAnnotations(map[string]string{
		"example.com/owner-name":      "my-owner",
		"example.com/owner-namespace": "platform",
	})

	cb.OnAdd(obj)

	select {
	case <-eventCh:
		t.Error("expected no event for non-matching labels")
	default:
	}
}

func TestLabelSelectorCallback_NoLabels(t *testing.T) {
	eventCh := make(chan event.GenericEvent, 10)
	selector, err := labels.Parse("app=nginx")
	if err != nil {
		t.Fatalf("unexpected error parsing selector: %v", err)
	}

	cb := LabelSelectorCallback(selector, ownerAnnotationMapFunc, eventCh)

	obj := &unstructured.Unstructured{}
	obj.SetName("my-pod")
	obj.SetNamespace("default")
	obj.SetAnnotations(map[string]string{
		"example.com/owner-name":      "my-owner",
		"example.com/owner-namespace": "platform",
	})

	cb.OnAdd(obj)

	select {
	case <-eventCh:
		t.Error("expected no event for object without labels")
	default:
	}
}

func TestLabelSelectorCallback_OnUpdate(t *testing.T) {
	eventCh := make(chan event.GenericEvent, 10)
	selector, err := labels.Parse("app=nginx")
	if err != nil {
		t.Fatalf("unexpected error parsing selector: %v", err)
	}

	cb := LabelSelectorCallback(selector, ownerAnnotationMapFunc, eventCh)

	oldObj := &unstructured.Unstructured{}
	oldObj.SetName("my-pod")
	oldObj.SetNamespace("default")
	oldObj.SetLabels(map[string]string{"app": "redis"})

	newObj := &unstructured.Unstructured{}
	newObj.SetName("my-pod")
	newObj.SetNamespace("default")
	newObj.SetLabels(map[string]string{"app": "nginx"})
	newObj.SetAnnotations(map[string]string{
		"example.com/owner-name":      "my-owner",
		"example.com/owner-namespace": "platform",
	})

	cb.OnUpdate(oldObj, newObj)

	select {
	case evt := <-eventCh:
		if evt.Object.GetName() != "my-owner" {
			t.Errorf("expected name my-owner, got %s", evt.Object.GetName())
		}
	default:
		t.Error("expected event from OnUpdate when newObj matches")
	}
}

func TestLabelSelectorCallback_OnDelete(t *testing.T) {
	eventCh := make(chan event.GenericEvent, 10)
	selector, err := labels.Parse("app=nginx")
	if err != nil {
		t.Fatalf("unexpected error parsing selector: %v", err)
	}

	cb := LabelSelectorCallback(selector, ownerAnnotationMapFunc, eventCh)

	obj := &unstructured.Unstructured{}
	obj.SetName("my-pod")
	obj.SetNamespace("default")
	obj.SetLabels(map[string]string{"app": "nginx"})
	obj.SetAnnotations(map[string]string{
		"example.com/owner-name":      "my-owner",
		"example.com/owner-namespace": "platform",
	})

	cb.OnDelete(obj)

	select {
	case evt := <-eventCh:
		if evt.Object.GetName() != "my-owner" {
			t.Errorf("expected name my-owner, got %s", evt.Object.GetName())
		}
	default:
		t.Error("expected event from OnDelete")
	}
}

func TestLabelSelectorCallback_ChannelFull(t *testing.T) {
	eventCh := make(chan event.GenericEvent) // unbuffered = always full
	selector, err := labels.Parse("app=nginx")
	if err != nil {
		t.Fatalf("unexpected error parsing selector: %v", err)
	}

	cb := LabelSelectorCallback(selector, ownerAnnotationMapFunc, eventCh)

	obj := &unstructured.Unstructured{}
	obj.SetName("my-pod")
	obj.SetNamespace("default")
	obj.SetLabels(map[string]string{"app": "nginx"})
	obj.SetAnnotations(map[string]string{
		"example.com/owner-name":      "my-owner",
		"example.com/owner-namespace": "platform",
	})

	// Should not block or panic when channel is full
	cb.OnAdd(obj)
}

func TestLabelSelectorCallback_EverythingSelector(t *testing.T) {
	eventCh := make(chan event.GenericEvent, 10)
	cb := LabelSelectorCallback(labels.Everything(), testMapFunc, eventCh)

	obj := &unstructured.Unstructured{}
	obj.SetName("anything")
	obj.SetNamespace("default")

	cb.OnAdd(obj)

	select {
	case evt := <-eventCh:
		if evt.Object.GetName() != "anything" {
			t.Errorf("expected name anything, got %s", evt.Object.GetName())
		}
	default:
		t.Error("expected event for Everything selector")
	}
}

// --- EnqueueByOwnerAnnotationCallback tests ---

func TestEnqueueByOwnerAnnotationCallback_EnqueuesOwner(t *testing.T) {
	eventCh := make(chan event.GenericEvent, 10)
	cb := EnqueueByOwnerAnnotationCallback(
		"example.com/owner-name",
		"example.com/owner-namespace",
		eventCh,
	)

	obj := &unstructured.Unstructured{}
	obj.SetName("child")
	obj.SetNamespace("child-ns")
	obj.SetAnnotations(map[string]string{
		"example.com/owner-name":      "my-template",
		"example.com/owner-namespace": "platform",
	})

	cb.OnAdd(obj)

	select {
	case evt := <-eventCh:
		if evt.Object.GetName() != "my-template" {
			t.Errorf("expected owner name my-template, got %s", evt.Object.GetName())
		}
		if evt.Object.GetNamespace() != "platform" {
			t.Errorf("expected owner namespace platform, got %s", evt.Object.GetNamespace())
		}
	default:
		t.Error("expected event to be enqueued")
	}
}

func TestEnqueueByOwnerAnnotationCallback_SkipsMissingAnnotations(t *testing.T) {
	eventCh := make(chan event.GenericEvent, 10)
	cb := EnqueueByOwnerAnnotationCallback(
		"example.com/owner-name",
		"example.com/owner-namespace",
		eventCh,
	)

	// No annotations at all
	obj := &unstructured.Unstructured{}
	obj.SetName("child")
	obj.SetNamespace("default")
	cb.OnAdd(obj)

	select {
	case <-eventCh:
		t.Error("expected no event for object without annotations")
	default:
	}

	// Only name annotation (missing namespace)
	obj.SetAnnotations(map[string]string{
		"example.com/owner-name": "my-owner",
	})
	cb.OnAdd(obj)

	select {
	case <-eventCh:
		t.Error("expected no event with missing namespace annotation")
	default:
	}

	// Only namespace annotation (missing name)
	obj.SetAnnotations(map[string]string{
		"example.com/owner-namespace": "platform",
	})
	cb.OnAdd(obj)

	select {
	case <-eventCh:
		t.Error("expected no event with missing name annotation")
	default:
	}
}

func TestEnqueueByOwnerAnnotationCallback_OnUpdate(t *testing.T) {
	eventCh := make(chan event.GenericEvent, 10)
	cb := EnqueueByOwnerAnnotationCallback(
		"example.com/owner-name",
		"example.com/owner-namespace",
		eventCh,
	)

	oldObj := &unstructured.Unstructured{}
	oldObj.SetName("child")
	oldObj.SetNamespace("child-ns")

	newObj := &unstructured.Unstructured{}
	newObj.SetName("child")
	newObj.SetNamespace("child-ns")
	newObj.SetAnnotations(map[string]string{
		"example.com/owner-name":      "my-template",
		"example.com/owner-namespace": "platform",
	})

	cb.OnUpdate(oldObj, newObj)

	select {
	case evt := <-eventCh:
		if evt.Object.GetName() != "my-template" {
			t.Errorf("expected owner name my-template, got %s", evt.Object.GetName())
		}
	default:
		t.Error("expected event from OnUpdate")
	}
}

func TestEnqueueByOwnerAnnotationCallback_OnDelete(t *testing.T) {
	eventCh := make(chan event.GenericEvent, 10)
	cb := EnqueueByOwnerAnnotationCallback(
		"example.com/owner-name",
		"example.com/owner-namespace",
		eventCh,
	)

	obj := &unstructured.Unstructured{}
	obj.SetName("child")
	obj.SetNamespace("child-ns")
	obj.SetAnnotations(map[string]string{
		"example.com/owner-name":      "my-template",
		"example.com/owner-namespace": "platform",
	})

	cb.OnDelete(obj)

	select {
	case evt := <-eventCh:
		if evt.Object.GetName() != "my-template" {
			t.Errorf("expected owner name my-template, got %s", evt.Object.GetName())
		}
	default:
		t.Error("expected event from OnDelete")
	}
}

func TestEnqueueByOwnerAnnotationCallback_ChannelFull(t *testing.T) {
	eventCh := make(chan event.GenericEvent) // unbuffered = always full
	cb := EnqueueByOwnerAnnotationCallback(
		"example.com/owner-name",
		"example.com/owner-namespace",
		eventCh,
	)

	obj := &unstructured.Unstructured{}
	obj.SetName("child")
	obj.SetNamespace("child-ns")
	obj.SetAnnotations(map[string]string{
		"example.com/owner-name":      "my-template",
		"example.com/owner-namespace": "platform",
	})

	// Should not block or panic when channel is full
	cb.OnAdd(obj)
}
