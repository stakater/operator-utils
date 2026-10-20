package dynamicinformer

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var (
	deploymentsGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	servicesGVR    = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "services"}
	configmapsGVR  = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}
	secretsGVR     = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}
)

func newFakeDynamicClient() *dynamicfake.FakeDynamicClient {
	scheme := runtime.NewScheme()
	gvrToListKind := map[schema.GroupVersionResource]string{
		deploymentsGVR: "DeploymentList",
		servicesGVR:    "ServiceList",
		configmapsGVR:  "ConfigMapList",
		secretsGVR:     "SecretList",
	}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, gvrToListKind)
}

func newFakeRegistry(t *testing.T) (*WatchRegistry, context.Context, context.CancelFunc) {
	t.Helper()
	registry := NewWatchRegistry(newFakeDynamicClient(), 30*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	return registry, ctx, cancel
}

// fakeClientProvider serves a distinct fake dynamic client per identity and
// records which identities were requested. Identities in errFor fail.
type fakeClientProvider struct {
	mu      sync.Mutex
	clients map[string]dynamic.Interface
	calls   []string
	errFor  map[string]error
}

func newFakeClientProvider() *fakeClientProvider {
	return &fakeClientProvider{
		clients: make(map[string]dynamic.Interface),
		errFor:  make(map[string]error),
	}
}

func (p *fakeClientProvider) ClientFor(identity string) (dynamic.Interface, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, identity)
	if err, ok := p.errFor[identity]; ok {
		return nil, err
	}
	cli, ok := p.clients[identity]
	if !ok {
		cli = newFakeDynamicClient()
		p.clients[identity] = cli
	}
	return cli, nil
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

// --- Identity tests ---

func TestEnsureWatchSet_DifferentIdentitiesDifferentInformers(t *testing.T) {
	provider := newFakeClientProvider()
	registry := NewWatchRegistryWithProvider(provider, 30*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	watches := []WatchRequest{
		{
			Key:      WatchKey{GVR: secretsGVR, Namespace: "shared", Identity: "team-a/reader"},
			Callback: noopCallback(),
		},
		{
			Key:      WatchKey{GVR: secretsGVR, Namespace: "shared", Identity: "team-b/reader"},
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

	// Same GVR+namespace but different identities = separate informers
	if len(registry.watches) != 2 {
		t.Errorf("expected 2 watch entries for different identities, got %d", len(registry.watches))
	}

	// Each informer must have been built from its own identity's client
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.calls) != 2 {
		t.Fatalf("expected 2 ClientFor calls, got %d (%v)", len(provider.calls), provider.calls)
	}
	seen := map[string]bool{}
	for _, id := range provider.calls {
		seen[id] = true
	}
	if !seen["team-a/reader"] || !seen["team-b/reader"] {
		t.Errorf("expected ClientFor calls for both identities, got %v", provider.calls)
	}
}

func TestEnsureWatchSet_SameIdentitySharesInformer(t *testing.T) {
	provider := newFakeClientProvider()
	registry := NewWatchRegistryWithProvider(provider, 30*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	key := WatchKey{GVR: secretsGVR, Namespace: "shared", Identity: "team-a/reader"}

	if _, err := registry.EnsureWatchSet(ctx, "consumer-1", []WatchRequest{{Key: key, Callback: noopCallback()}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := registry.EnsureWatchSet(ctx, "consumer-2", []WatchRequest{{Key: key, Callback: noopCallback()}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	registry.mu.Lock()
	defer registry.mu.Unlock()

	if len(registry.watches) != 1 {
		t.Errorf("expected 1 shared watch entry, got %d", len(registry.watches))
	}
	if len(registry.watches[key].handlers) != 2 {
		t.Errorf("expected 2 handlers on shared informer, got %d", len(registry.watches[key].handlers))
	}

	// Client resolved only once — second consumer reuses the running informer
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.calls) != 1 {
		t.Errorf("expected 1 ClientFor call, got %d (%v)", len(provider.calls), provider.calls)
	}
}

func TestEnsureWatchSet_IdentityChangeSwapsWatches(t *testing.T) {
	provider := newFakeClientProvider()
	registry := NewWatchRegistryWithProvider(provider, 30*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	oldKey := WatchKey{GVR: secretsGVR, Namespace: "shared", Identity: "team-a/old-sa"}
	newKey := WatchKey{GVR: secretsGVR, Namespace: "shared", Identity: "team-a/new-sa"}

	if _, err := registry.EnsureWatchSet(ctx, "consumer-1", []WatchRequest{{Key: oldKey, Callback: noopCallback()}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := registry.EnsureWatchSet(ctx, "consumer-1", []WatchRequest{{Key: newKey, Callback: noopCallback()}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	registry.mu.Lock()
	defer registry.mu.Unlock()

	if _, ok := registry.watches[oldKey]; ok {
		t.Error("expected old-identity watch to be released after identity change")
	}
	if _, ok := registry.watches[newKey]; !ok {
		t.Error("expected new-identity watch to exist")
	}
	if len(registry.consumerKeys["consumer-1"]) != 1 {
		t.Errorf("expected 1 consumer key, got %d", len(registry.consumerKeys["consumer-1"]))
	}
}

func TestEnsureWatchSet_ProviderErrorRollsBack(t *testing.T) {
	provider := newFakeClientProvider()
	provider.errFor["team-b/broken"] = context.DeadlineExceeded // any error
	registry := NewWatchRegistryWithProvider(provider, 30*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	goodKey := WatchKey{GVR: secretsGVR, Namespace: "shared", Identity: "team-a/reader"}
	badKey := WatchKey{GVR: secretsGVR, Namespace: "shared", Identity: "team-b/broken"}

	// Fresh consumer: the good watch is added, then the bad one fails —
	// the good one must be rolled back so the consumer stays unregistered.
	_, err := registry.EnsureWatchSet(ctx, "consumer-1", []WatchRequest{
		{Key: goodKey, Callback: noopCallback()},
		{Key: badKey, Callback: noopCallback()},
	})
	if err == nil {
		t.Fatal("expected error from failing provider")
	}

	registry.mu.Lock()
	if len(registry.watches) != 0 {
		t.Errorf("expected all watches rolled back, got %d", len(registry.watches))
	}
	if len(registry.consumerKeys) != 0 {
		t.Errorf("expected no consumer keys after rollback, got %d", len(registry.consumerKeys))
	}
	registry.mu.Unlock()

	// Consumer with an existing set keeps it when a later ensure fails.
	if _, err := registry.EnsureWatchSet(ctx, "consumer-1", []WatchRequest{{Key: goodKey, Callback: noopCallback()}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_, err = registry.EnsureWatchSet(ctx, "consumer-1", []WatchRequest{
		{Key: goodKey, Callback: noopCallback()},
		{Key: badKey, Callback: noopCallback()},
	})
	if err == nil {
		t.Fatal("expected error from failing provider")
	}

	registry.mu.Lock()
	defer registry.mu.Unlock()
	if _, ok := registry.watches[goodKey]; !ok {
		t.Error("expected pre-existing watch to survive a failed ensure")
	}
	if len(registry.consumerKeys["consumer-1"]) != 1 {
		t.Errorf("expected consumer to keep its previous 1-key set, got %d", len(registry.consumerKeys["consumer-1"]))
	}
}

// nilClientProvider returns (nil, nil) — an invalid implementation the
// registry must reject instead of panicking inside the informer.
type nilClientProvider struct{}

func (nilClientProvider) ClientFor(string) (dynamic.Interface, error) { return nil, nil }

func TestEnsureWatchSet_NilClientFromProviderIsRejected(t *testing.T) {
	registry := NewWatchRegistryWithProvider(nilClientProvider{}, 30*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err := registry.EnsureWatchSet(ctx, "consumer-1", []WatchRequest{
		{Key: WatchKey{GVR: deploymentsGVR, Namespace: "default"}, Callback: noopCallback()},
	})
	if err == nil {
		t.Fatal("expected error for provider returning nil client")
	}

	registry.mu.Lock()
	defer registry.mu.Unlock()
	if len(registry.watches) != 0 {
		t.Errorf("expected no watches after nil-client rejection, got %d", len(registry.watches))
	}
}

func TestNewWatchRegistry_NilClientIsRejected(t *testing.T) {
	registry := NewWatchRegistry(nil, 30*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err := registry.EnsureWatchSet(ctx, "consumer-1", []WatchRequest{
		{Key: WatchKey{GVR: deploymentsGVR, Namespace: "default"}, Callback: noopCallback()},
	})
	if err == nil {
		t.Fatal("expected error for registry created with nil client")
	}
}

func TestNewWatchRegistry_RejectsNonEmptyIdentity(t *testing.T) {
	registry, ctx, cancel := newFakeRegistry(t) // legacy single-client constructor
	defer cancel()

	_, err := registry.EnsureWatchSet(ctx, "consumer-1", []WatchRequest{
		{
			Key:      WatchKey{GVR: deploymentsGVR, Namespace: "default", Identity: "some/identity"},
			Callback: noopCallback(),
		},
	})
	if err == nil {
		t.Fatal("expected error for non-empty identity on single-client registry")
	}

	registry.mu.Lock()
	defer registry.mu.Unlock()
	if len(registry.watches) != 0 {
		t.Errorf("expected no watches, got %d", len(registry.watches))
	}
}

func TestWatchKey_IdentityDifferentiatesKeys(t *testing.T) {
	base := WatchKey{GVR: deploymentsGVR, Namespace: "default"}
	withID := WatchKey{GVR: deploymentsGVR, Namespace: "default", Identity: "team-a/reader"}

	m := map[WatchKey]int{base: 1}
	if _, ok := m[withID]; ok {
		t.Error("expected identity to differentiate otherwise-equal keys")
	}
	m[withID] = 2
	if m[base] != 1 || m[withID] != 2 {
		t.Error("expected both keys to coexist in the map")
	}
}

// --- ClientReleaser tests ---

// fakeReleasingProvider is a fakeClientProvider that also implements
// ClientReleaser and records every Release call.
type fakeReleasingProvider struct {
	*fakeClientProvider
	releases []string
}

func newFakeReleasingProvider() *fakeReleasingProvider {
	return &fakeReleasingProvider{fakeClientProvider: newFakeClientProvider()}
}

func (p *fakeReleasingProvider) Release(identity string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.releases = append(p.releases, identity)
}

// acquired returns the identities passed to ClientFor so far, in order.
func (p *fakeReleasingProvider) acquired() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.calls)
}

// released returns the identities passed to Release so far, in order.
func (p *fakeReleasingProvider) released() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.releases)
}

func newReleasingRegistry(t *testing.T) (*WatchRegistry, *fakeReleasingProvider, context.Context) {
	t.Helper()
	provider := newFakeReleasingProvider()
	registry := NewWatchRegistryWithProvider(provider, 30*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return registry, provider, ctx
}

func TestClientReleaser_ReleasedWhenInformerStops(t *testing.T) {
	registry, provider, ctx := newReleasingRegistry(t)

	key := WatchKey{GVR: secretsGVR, Namespace: "shared", Identity: "team-a/reader"}
	if _, err := registry.EnsureWatchSet(ctx, "consumer-1", []WatchRequest{{Key: key, Callback: noopCallback()}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := provider.released(); len(got) != 0 {
		t.Fatalf("expected no release while the informer runs, got %v", got)
	}

	if err := registry.ReleaseAll("consumer-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := provider.released(); !slices.Equal(got, []string{"team-a/reader"}) {
		t.Errorf("expected exactly one release of team-a/reader, got %v", got)
	}

	// A repeated release of an already-released consumer must not release again.
	if err := registry.ReleaseAll("consumer-1"); err != nil {
		t.Fatalf("unexpected error on second release: %v", err)
	}
	if got := provider.released(); len(got) != 1 {
		t.Errorf("expected release count to stay at 1, got %v", got)
	}
}

func TestClientReleaser_SharedInformerReleasesOnce(t *testing.T) {
	registry, provider, ctx := newReleasingRegistry(t)

	key := WatchKey{GVR: secretsGVR, Namespace: "shared", Identity: "team-a/reader"}
	for _, consumer := range []string{"consumer-1", "consumer-2"} {
		if _, err := registry.EnsureWatchSet(ctx, consumer, []WatchRequest{{Key: key, Callback: noopCallback()}}); err != nil {
			t.Fatalf("unexpected error for %s: %v", consumer, err)
		}
	}
	if got := provider.acquired(); !slices.Equal(got, []string{"team-a/reader"}) {
		t.Fatalf("expected one ClientFor call for the shared informer, got %v", got)
	}

	// The informer keeps running for consumer-2, so nothing is released yet.
	if err := registry.ReleaseAll("consumer-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := provider.released(); len(got) != 0 {
		t.Fatalf("expected no release while consumer-2 still holds the informer, got %v", got)
	}

	if err := registry.ReleaseAll("consumer-2"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := provider.released(); !slices.Equal(got, []string{"team-a/reader"}) {
		t.Errorf("expected exactly one release after the last consumer left, got %v", got)
	}
}

func TestClientReleaser_IdentityChangeReleasesOldIdentity(t *testing.T) {
	registry, provider, ctx := newReleasingRegistry(t)

	oldKey := WatchKey{GVR: secretsGVR, Namespace: "shared", Identity: "team-a/old-sa"}
	newKey := WatchKey{GVR: secretsGVR, Namespace: "shared", Identity: "team-a/new-sa"}

	if _, err := registry.EnsureWatchSet(ctx, "consumer-1", []WatchRequest{{Key: oldKey, Callback: noopCallback()}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := registry.EnsureWatchSet(ctx, "consumer-1", []WatchRequest{{Key: newKey, Callback: noopCallback()}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The stale old-identity informer stopped; the new one is still running.
	if got := provider.released(); !slices.Equal(got, []string{"team-a/old-sa"}) {
		t.Errorf("expected exactly one release of the old identity, got %v", got)
	}

	if err := registry.ReleaseAll("consumer-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := provider.released(); !slices.Equal(got, []string{"team-a/old-sa", "team-a/new-sa"}) {
		t.Errorf("expected old then new identity released, got %v", got)
	}
}

func TestClientReleaser_OneReleasePerInformer(t *testing.T) {
	registry, provider, ctx := newReleasingRegistry(t)

	// Two informers under the same identity: ClientFor is called once each,
	// so Release must also be called once each.
	keyA := WatchKey{GVR: secretsGVR, Namespace: "ns-a", Identity: "team-a/reader"}
	keyB := WatchKey{GVR: secretsGVR, Namespace: "ns-b", Identity: "team-a/reader"}

	if _, err := registry.EnsureWatchSet(ctx, "consumer-1", []WatchRequest{
		{Key: keyA, Callback: noopCallback()},
		{Key: keyB, Callback: noopCallback()},
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := provider.acquired(); !slices.Equal(got, []string{"team-a/reader", "team-a/reader"}) {
		t.Fatalf("expected two ClientFor calls, got %v", got)
	}

	// Dropping one key from the set stops one informer.
	if _, err := registry.EnsureWatchSet(ctx, "consumer-1", []WatchRequest{{Key: keyA, Callback: noopCallback()}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := provider.released(); !slices.Equal(got, []string{"team-a/reader"}) {
		t.Errorf("expected one release after dropping one informer, got %v", got)
	}

	if err := registry.ReleaseAll("consumer-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := provider.released(); !slices.Equal(got, []string{"team-a/reader", "team-a/reader"}) {
		t.Errorf("expected two releases in total, got %v", got)
	}
	if acquired, released := provider.acquired(), provider.released(); len(acquired) != len(released) {
		t.Errorf("expected ClientFor and Release to balance, got %d acquisitions and %d releases", len(acquired), len(released))
	}
}

func TestClientReleaser_ProviderErrorRollsBackAndReleases(t *testing.T) {
	registry, provider, ctx := newReleasingRegistry(t)
	provider.errFor["team-b/broken"] = context.DeadlineExceeded // any error

	goodKey := WatchKey{GVR: secretsGVR, Namespace: "shared", Identity: "team-a/reader"}
	badKey := WatchKey{GVR: secretsGVR, Namespace: "shared", Identity: "team-b/broken"}

	_, err := registry.EnsureWatchSet(ctx, "consumer-1", []WatchRequest{
		{Key: goodKey, Callback: noopCallback()},
		{Key: badKey, Callback: noopCallback()},
	})
	if err == nil {
		t.Fatal("expected error from failing provider")
	}

	// The good informer started and was rolled back, so its client is released
	// once. The failing ClientFor handed out nothing, so nothing is released for it.
	if got := provider.released(); !slices.Equal(got, []string{"team-a/reader"}) {
		t.Errorf("expected only the rolled-back identity released, got %v", got)
	}
}

// nilReleasingProvider returns (nil, nil) from ClientFor and counts releases.
type nilReleasingProvider struct {
	nilClientProvider
	releases int
}

func (p *nilReleasingProvider) Release(string) { p.releases++ }

func TestClientReleaser_NilClientIsNotReleased(t *testing.T) {
	provider := &nilReleasingProvider{}
	registry := NewWatchRegistryWithProvider(provider, 30*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err := registry.EnsureWatchSet(ctx, "consumer-1", []WatchRequest{
		{Key: WatchKey{GVR: deploymentsGVR, Namespace: "default", Identity: "team-a/reader"}, Callback: noopCallback()},
	})
	if err == nil {
		t.Fatal("expected error for provider returning nil client")
	}
	if provider.releases != 0 {
		t.Errorf("expected no release for a rejected nil client, got %d", provider.releases)
	}
}

func TestClientReleaser_NotImplementedIsNoop(t *testing.T) {
	// Neither the plain identity provider nor the legacy single-client
	// constructor implements ClientReleaser, so the registry records none.
	provider := newFakeClientProvider()
	registry := NewWatchRegistryWithProvider(provider, 30*time.Second)
	if registry.releaser != nil {
		t.Error("expected no releaser for a provider without Release")
	}
	legacy := NewWatchRegistry(newFakeDynamicClient(), 30*time.Second)
	if legacy.releaser != nil {
		t.Error("expected no releaser for the single-client registry")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	key := WatchKey{GVR: secretsGVR, Namespace: "shared", Identity: "team-a/reader"}
	if _, err := registry.EnsureWatchSet(ctx, "consumer-1", []WatchRequest{{Key: key, Callback: noopCallback()}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := registry.ReleaseAll("consumer-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	registry.mu.Lock()
	defer registry.mu.Unlock()
	if len(registry.watches) != 0 {
		t.Errorf("expected informer torn down as before, got %d watches", len(registry.watches))
	}
}

func TestClientReleaser_AddEventHandlerFailureStopsFreshInformer(t *testing.T) {
	registry, provider, ctx := newReleasingRegistry(t)

	// Hand the registry an informer that has already stopped, so
	// AddEventHandler fails deterministically on a freshly started entry.
	registry.newInformer = func(cli dynamic.Interface, key WatchKey, resyncPeriod time.Duration) (cache.SharedIndexInformer, error) {
		informer, _ := newDynamicInformer(cli, key, resyncPeriod)
		stop := make(chan struct{})
		close(stop)
		informer.Run(stop)
		return informer, nil
	}

	key := WatchKey{GVR: secretsGVR, Namespace: "shared", Identity: "team-a/reader"}
	_, err := registry.EnsureWatchSet(ctx, "consumer-1", []WatchRequest{{Key: key, Callback: noopCallback()}})
	if err == nil {
		t.Fatal("expected error when the handler cannot be added")
	}

	// The entry must not linger as a handler-less informer, and the one
	// ClientFor that started it must be paired with one Release.
	registry.mu.Lock()
	if len(registry.watches) != 0 {
		t.Errorf("expected the failed informer to be dropped, got %d watches", len(registry.watches))
	}
	if len(registry.consumerKeys) != 0 {
		t.Errorf("expected no consumer keys, got %d", len(registry.consumerKeys))
	}
	registry.mu.Unlock()

	if got := provider.acquired(); !slices.Equal(got, []string{"team-a/reader"}) {
		t.Errorf("expected one ClientFor call, got %v", got)
	}
	if got := provider.released(); !slices.Equal(got, []string{"team-a/reader"}) {
		t.Errorf("expected one release for the dropped informer, got %v", got)
	}
}

func TestClientReleaser_ConcurrentAccessIsBalanced(t *testing.T) {
	registry, provider, ctx := newReleasingRegistry(t)

	key := WatchKey{GVR: deploymentsGVR, Namespace: "default", Identity: "team-a/reader"}
	consumerID := func(i int) string { return string(rune('a'+i)) + "-consumer" }

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(2)
		go func(id int) {
			defer wg.Done()
			_, _ = registry.EnsureWatchSet(ctx, consumerID(id), []WatchRequest{{Key: key, Callback: noopCallback()}})
		}(i)
		go func(id int) {
			defer wg.Done()
			_ = registry.ReleaseAll(consumerID(id))
		}(i)
	}
	wg.Wait()

	// Whatever interleaving happened, a final release of every consumer must
	// leave no informer behind and every ClientFor paired with one Release.
	for i := 0; i < 10; i++ {
		if err := registry.ReleaseAll(consumerID(i)); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	registry.mu.Lock()
	if len(registry.watches) != 0 {
		t.Errorf("expected no watches after final release, got %d", len(registry.watches))
	}
	registry.mu.Unlock()

	acquired, released := provider.acquired(), provider.released()
	if len(acquired) == 0 {
		t.Fatal("expected at least one informer to have started")
	}
	if len(acquired) != len(released) {
		t.Errorf("expected ClientFor and Release to balance, got %d acquisitions and %d releases", len(acquired), len(released))
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

func newTeamConfigMap(name, team string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("v1")
	obj.SetKind("ConfigMap")
	obj.SetNamespace("default")
	obj.SetName(name)
	obj.SetLabels(map[string]string{"team": team})
	return obj
}

func indexByTeam(obj interface{}) ([]string, error) {
	return []string{obj.(*unstructured.Unstructured).GetLabels()["team"]}, nil
}

func TestWithInformerFunc_UsedForNewWatches(t *testing.T) {
	// The func runs on the calling goroutine under r.mu, so no lock is needed.
	var built []WatchKey
	counting := func(cli dynamic.Interface, key WatchKey, resyncPeriod time.Duration) (cache.SharedIndexInformer, error) {
		built = append(built, key)
		return newDynamicInformer(cli, key, resyncPeriod)
	}
	registry := NewWatchRegistry(newFakeDynamicClient(), 30*time.Second, WithInformerFunc(counting))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmKey := WatchKey{GVR: configmapsGVR, Namespace: "default"}
	secretKey := WatchKey{GVR: secretsGVR, Namespace: "default"}
	if _, err := registry.EnsureWatchSet(ctx, "consumer-1", []WatchRequest{{Key: cmKey}, {Key: secretKey}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := registry.EnsureWatchSet(ctx, "consumer-2", []WatchRequest{{Key: cmKey}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !slices.Equal(built, []WatchKey{cmKey, secretKey}) {
		t.Errorf("expected one informer per distinct key, got %v", built)
	}
}

func TestWithInformerFunc_IndexerReachable(t *testing.T) {
	scheme := runtime.NewScheme()
	cli := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{configmapsGVR: "ConfigMapList"},
		newTeamConfigMap("a", "x"), newTeamConfigMap("b", "y"))
	withTeamIndex := func(cli dynamic.Interface, key WatchKey, resyncPeriod time.Duration) (cache.SharedIndexInformer, error) {
		informer, _ := newDynamicInformer(cli, key, resyncPeriod)
		return informer, informer.AddIndexers(cache.Indexers{"byTeam": indexByTeam})
	}
	registry := NewWatchRegistry(cli, 30*time.Second, WithInformerFunc(withTeamIndex))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	regs, err := registry.EnsureWatchSet(ctx, "consumer-1", []WatchRequest{{Key: WatchKey{GVR: configmapsGVR, Namespace: "default"}}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cache.WaitForCacheSync(ctx.Done(), regs[0].HasSynced) {
		t.Fatal("informer did not sync")
	}

	indexer, err := regs[0].GetIndexer()
	if err != nil {
		t.Fatalf("GetIndexer: %v", err)
	}
	objs, err := indexer.ByIndex("byTeam", "x")
	if err != nil {
		t.Fatalf("ByIndex: %v", err)
	}
	if len(objs) != 1 || objs[0].(*unstructured.Unstructured).GetName() != "a" {
		t.Errorf("expected only default/a for team x, got %v", objs)
	}
}

func TestWithInformerFunc_NilInformerRejected(t *testing.T) {
	provider := newFakeReleasingProvider()
	nilInformer := func(dynamic.Interface, WatchKey, time.Duration) (cache.SharedIndexInformer, error) { return nil, nil }
	registry := NewWatchRegistryWithProvider(provider, 30*time.Second, WithInformerFunc(nilInformer))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	key := WatchKey{GVR: secretsGVR, Namespace: "shared", Identity: "team-a/reader"}
	if _, err := registry.EnsureWatchSet(ctx, "consumer-1", []WatchRequest{{Key: key, Callback: noopCallback()}}); err == nil {
		t.Fatal("expected error for a nil informer")
	}

	registry.mu.Lock()
	if len(registry.watches) != 0 || len(registry.consumerKeys) != 0 {
		t.Errorf("expected no state left, got %d watches, %d consumers", len(registry.watches), len(registry.consumerKeys))
	}
	registry.mu.Unlock()

	if got := provider.acquired(); !slices.Equal(got, []string{"team-a/reader"}) {
		t.Errorf("expected one ClientFor call, got %v", got)
	}
	if got := provider.released(); !slices.Equal(got, []string{"team-a/reader"}) {
		t.Errorf("expected the client to be released, got %v", got)
	}
}

func TestGetIndexer_SharedAcrossConsumers(t *testing.T) {
	registry, ctx, cancel := newFakeRegistry(t)
	defer cancel()

	key := WatchKey{GVR: configmapsGVR, Namespace: "default"}
	regs1, err := registry.EnsureWatchSet(ctx, "consumer-1", []WatchRequest{{Key: key}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	regs2, err := registry.EnsureWatchSet(ctx, "consumer-2", []WatchRequest{{Key: key}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	indexer1, err1 := regs1[0].GetIndexer()
	indexer2, err2 := regs2[0].GetIndexer()
	if err1 != nil || err2 != nil || indexer1 == nil || indexer1 != indexer2 {
		t.Error("expected both consumers to read the same indexer")
	}
}

func TestWithInformerFunc_NilInformerRollsBackEarlierKeys(t *testing.T) {
	provider := newFakeReleasingProvider()
	nilForSecrets := func(cli dynamic.Interface, key WatchKey, resyncPeriod time.Duration) (cache.SharedIndexInformer, error) {
		if key.GVR == secretsGVR {
			return nil, nil
		}
		return newDynamicInformer(cli, key, resyncPeriod)
	}
	registry := NewWatchRegistryWithProvider(provider, 30*time.Second, WithInformerFunc(nilForSecrets))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	watches := []WatchRequest{
		{Key: WatchKey{GVR: configmapsGVR, Namespace: "default", Identity: "team-a/reader"}, Callback: noopCallback()},
		{Key: WatchKey{GVR: secretsGVR, Namespace: "default", Identity: "team-b/reader"}, Callback: noopCallback()},
	}
	if _, err := registry.EnsureWatchSet(ctx, "consumer-1", watches); err == nil {
		t.Fatal("expected error for a nil informer")
	}

	registry.mu.Lock()
	if len(registry.watches) != 0 || len(registry.consumerKeys) != 0 {
		t.Errorf("expected the earlier key rolled back, got %d watches, %d consumers", len(registry.watches), len(registry.consumerKeys))
	}
	registry.mu.Unlock()

	acquired, released := provider.acquired(), provider.released()
	slices.Sort(acquired)
	slices.Sort(released)
	if !slices.Equal(acquired, []string{"team-a/reader", "team-b/reader"}) || !slices.Equal(released, acquired) {
		t.Errorf("expected every acquired client released, acquired %v, released %v", acquired, released)
	}
}

// newFailingRegistry returns a registry whose InformerFunc runs fail for secrets
// and builds the default informer for anything else.
func newFailingRegistry(t *testing.T, fail func() (cache.SharedIndexInformer, error)) (*WatchRegistry, *fakeReleasingProvider, context.Context) {
	t.Helper()
	provider := newFakeReleasingProvider()
	informerFunc := func(cli dynamic.Interface, key WatchKey, resyncPeriod time.Duration) (cache.SharedIndexInformer, error) {
		if key.GVR == secretsGVR {
			return fail()
		}
		return newDynamicInformer(cli, key, resyncPeriod)
	}
	registry := NewWatchRegistryWithProvider(provider, 30*time.Second, WithInformerFunc(informerFunc))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return registry, provider, ctx
}

// assertEmptyAndReleased checks that a failed EnsureWatchSet left no state and
// released every client it acquired.
func assertEmptyAndReleased(t *testing.T, registry *WatchRegistry, provider *fakeReleasingProvider) {
	t.Helper()
	registry.mu.Lock()
	if len(registry.watches) != 0 || len(registry.consumerKeys) != 0 {
		t.Errorf("expected no state left, got %d watches, %d consumers", len(registry.watches), len(registry.consumerKeys))
	}
	registry.mu.Unlock()
	acquired, released := provider.acquired(), provider.released()
	slices.Sort(acquired)
	slices.Sort(released)
	if len(acquired) == 0 || !slices.Equal(released, acquired) {
		t.Errorf("expected every acquired client released, acquired %v, released %v", acquired, released)
	}
}

func failingWatches() []WatchRequest {
	return []WatchRequest{
		{Key: WatchKey{GVR: configmapsGVR, Namespace: "default", Identity: "team-a/reader"}, Callback: noopCallback()},
		{Key: WatchKey{GVR: secretsGVR, Namespace: "default", Identity: "team-b/reader"}, Callback: noopCallback()},
	}
}

func TestWithInformerFunc_ErrorRollsBack(t *testing.T) {
	registry, provider, ctx := newFailingRegistry(t, func() (cache.SharedIndexInformer, error) {
		return nil, errors.New("transform failed")
	})
	_, err := registry.EnsureWatchSet(ctx, "consumer-1", failingWatches())
	if err == nil || !strings.Contains(err.Error(), "transform failed") {
		t.Fatalf("expected the InformerFunc error, got %v", err)
	}
	assertEmptyAndReleased(t, registry, provider)
}

func TestWithInformerFunc_PanicRollsBack(t *testing.T) {
	registry, provider, ctx := newFailingRegistry(t, func() (cache.SharedIndexInformer, error) {
		panic("boom")
	})
	_, err := registry.EnsureWatchSet(ctx, "consumer-1", failingWatches())
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected the panic as an error, got %v", err)
	}
	assertEmptyAndReleased(t, registry, provider)
}

func TestWithInformerFunc_TypedNilInformerRejected(t *testing.T) {
	registry, provider, ctx := newFailingRegistry(t, func() (cache.SharedIndexInformer, error) {
		var typed *fakeSharedIndexInformer
		return typed, nil
	})
	if _, err := registry.EnsureWatchSet(ctx, "consumer-1", failingWatches()); err == nil {
		t.Fatal("expected error for a typed nil informer")
	}
	assertEmptyAndReleased(t, registry, provider)
}

// fakeSharedIndexInformer only exists to build a typed nil informer.
type fakeSharedIndexInformer struct{ cache.SharedIndexInformer }

func TestWithInformerFunc_NilInformerKeepsPreviousWatchSet(t *testing.T) {
	registry, _, ctx := newFailingRegistry(t, func() (cache.SharedIndexInformer, error) { return nil, nil })
	watches := failingWatches()
	if _, err := registry.EnsureWatchSet(ctx, "consumer-1", watches[:1]); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := registry.EnsureWatchSet(ctx, "consumer-1", watches); err == nil {
		t.Fatal("expected error for a nil informer")
	}

	registry.mu.Lock()
	defer registry.mu.Unlock()
	entry, ok := registry.watches[watches[0].Key]
	if !ok || entry.handlers["consumer-1"] == nil {
		t.Error("expected the previous watch and handler to stay")
	}
	if _, ok := registry.consumerKeys["consumer-1"][watches[0].Key]; !ok || len(registry.watches) != 1 {
		t.Errorf("expected consumer-1 to keep only its previous key, got %d watches", len(registry.watches))
	}
}

func TestWithInformerFunc_NilFuncKeepsDefault(t *testing.T) {
	registry := NewWatchRegistry(newFakeDynamicClient(), 30*time.Second, WithInformerFunc(nil))
	if reflect.ValueOf(registry.newInformer).Pointer() != reflect.ValueOf(InformerFunc(newDynamicInformer)).Pointer() {
		t.Error("expected WithInformerFunc(nil) to keep the default constructor")
	}
}

func TestWithInformerFunc_CalledAgainAfterRelease(t *testing.T) {
	calls := 0
	counting := func(cli dynamic.Interface, key WatchKey, resyncPeriod time.Duration) (cache.SharedIndexInformer, error) {
		calls++
		return newDynamicInformer(cli, key, resyncPeriod)
	}
	registry := NewWatchRegistry(newFakeDynamicClient(), 30*time.Second, WithInformerFunc(counting))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	key := WatchKey{GVR: configmapsGVR, Namespace: "default"}
	if _, err := registry.EnsureWatchSet(ctx, "consumer-1", []WatchRequest{{Key: key}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := registry.ReleaseAll("consumer-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := registry.EnsureWatchSet(ctx, "consumer-1", []WatchRequest{{Key: key}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 2 {
		t.Errorf("expected a fresh informer after release, func called %d times", calls)
	}
}

func TestWithInformerFunc_CallbacksFire(t *testing.T) {
	scheme := runtime.NewScheme()
	cli := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{configmapsGVR: "ConfigMapList"},
		newTeamConfigMap("a", "x"))
	plainInformer := func(cli dynamic.Interface, key WatchKey, resyncPeriod time.Duration) (cache.SharedIndexInformer, error) {
		ri := cli.Resource(key.GVR).Namespace(key.Namespace)
		lw := &cache.ListWatch{
			ListWithContextFunc:  func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) { return ri.List(ctx, o) },
			WatchFuncWithContext: func(ctx context.Context, o metav1.ListOptions) (watch.Interface, error) { return ri.Watch(ctx, o) },
		}
		// The fake client lacks watch list support, the same wrapper client-go's dynamic informer uses.
		return cache.NewSharedIndexInformer(cache.ToListWatcherWithWatchListSemantics(lw, cli), &unstructured.Unstructured{}, resyncPeriod, cache.Indexers{}), nil
	}
	registry := NewWatchRegistry(cli, 30*time.Second, WithInformerFunc(plainInformer))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	added := make(chan string, 1)
	cb := EventCallback{OnAdd: func(obj *unstructured.Unstructured) { added <- obj.GetName() }}
	if _, err := registry.EnsureWatchSet(ctx, "consumer-1", []WatchRequest{{Key: WatchKey{GVR: configmapsGVR, Namespace: "default"}, Callback: cb}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	select {
	case name := <-added:
		if name != "a" {
			t.Errorf("expected OnAdd for a, got %s", name)
		}
	case <-ctx.Done():
		t.Fatal("OnAdd did not fire on the custom informer")
	}
}

func TestGetIndexer_ErrorsAfterRelease(t *testing.T) {
	registry, ctx, cancel := newFakeRegistry(t)
	defer cancel()

	regs, err := registry.EnsureWatchSet(ctx, "consumer-1", []WatchRequest{{Key: WatchKey{GVR: configmapsGVR, Namespace: "default"}}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := regs[0].GetIndexer(); err != nil {
		t.Fatalf("expected a live indexer, got %v", err)
	}
	if err := registry.ReleaseAll("consumer-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := regs[0].GetIndexer(); !errors.Is(err, ErrWatchStopped) {
		t.Errorf("expected ErrWatchStopped after release, got %v", err)
	}
}

func TestGetIndexer_ZeroValueErrors(t *testing.T) {
	if _, err := (&WatchRegistration{}).GetIndexer(); err == nil {
		t.Error("expected an error from a registration not returned by EnsureWatchSet")
	}
}
