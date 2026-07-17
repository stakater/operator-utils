package dynamicinformer

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	kubedynamicinformer "k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
)

// WatchKey uniquely identifies a watch by GVR, namespace, and identity.
// An empty namespace means cluster-scoped.
//
// Identity selects which client the informer is started with (see ClientProvider)
// and is part of the key on purpose: watches under different identities are never
// shared, since they may be authorized differently. The registry treats the value
// as opaque; an empty identity means the registry's default client.
type WatchKey struct {
	GVR       schema.GroupVersionResource
	Namespace string
	Identity  string
}

// WatchRequest pairs a watch key with its callback. Passed as a slice to EnsureWatchSet.
type WatchRequest struct {
	Key      WatchKey
	Callback EventCallback
}

// EventCallback defines handlers for watch events. Callbacks are registered
// once per consumer per key and are NOT updated on subsequent calls.
// Callbacks must not capture call-site-scoped state; instead, derive context
// from the event object and stable references (client, channel, etc.).
type EventCallback struct {
	OnAdd    func(obj *unstructured.Unstructured)
	OnUpdate func(oldObj, newObj *unstructured.Unstructured)
	OnDelete func(obj *unstructured.Unstructured)
}

type handlerRegistration struct {
	registration cache.ResourceEventHandlerRegistration
}

// WatchEntry tracks an active informer and its per-consumer handler registrations.
type WatchEntry struct {
	cancel   context.CancelFunc
	informer cache.SharedIndexInformer
	handlers map[string]*handlerRegistration // keyed by consumerID
}

// WatchRegistration is returned to the consumer as a handle for optional cache sync.
type WatchRegistration struct {
	Key        WatchKey
	ConsumerID string
	HasSynced  cache.InformerSynced
}

// ClientProvider supplies the dynamic client for a given identity. The identity
// string is opaque to the registry — which credentials or cluster it maps to is
// entirely up to the implementation. An empty identity must return the default
// client. Implementations must never return (nil, nil); the registry rejects a
// nil client defensively.
//
// Implementations must be safe for concurrent use and should cache clients per
// identity, since ClientFor is called every time an informer starts.
type ClientProvider interface {
	ClientFor(identity string) (dynamic.Interface, error)
}

// singleClientProvider serves one fixed client for the empty identity only.
// It backs NewWatchRegistry for callers that predate identities.
type singleClientProvider struct {
	client dynamic.Interface
}

func (p singleClientProvider) ClientFor(identity string) (dynamic.Interface, error) {
	if identity != "" {
		return nil, fmt.Errorf("identity %q is not supported: this registry serves only the default identity (use NewWatchRegistryWithProvider for per-identity clients)", identity)
	}
	if p.client == nil {
		return nil, fmt.Errorf("registry was created with a nil dynamic client")
	}
	return p.client, nil
}

// WatchRegistry manages dynamic informers for arbitrary GroupVersionResource types
// at runtime. Each consumer registers callbacks per watch key. The registry deduplicates
// informers, manages handler lifecycle, and cleans up stale watches automatically.
//
// Safe for concurrent use.
type WatchRegistry struct {
	mu           sync.Mutex
	watches      map[WatchKey]*WatchEntry
	consumerKeys map[string]map[WatchKey]struct{} // consumerID → set of WatchKeys
	clients      ClientProvider
	resyncPeriod time.Duration
}

// NewWatchRegistry creates a new registry backed by the given dynamic client.
// All watch keys must use the empty identity. Use NewWatchRegistryWithProvider
// to serve watches under multiple identities.
// The resyncPeriod controls how often each informer relists from the API server.
// A non-zero value is required to recover from dropped events (30s is typical).
func NewWatchRegistry(dynamicClient dynamic.Interface, resyncPeriod time.Duration) *WatchRegistry {
	return NewWatchRegistryWithProvider(singleClientProvider{client: dynamicClient}, resyncPeriod)
}

// NewWatchRegistryWithProvider creates a registry that resolves the client for
// each watch from the provider, keyed by WatchKey.Identity.
func NewWatchRegistryWithProvider(provider ClientProvider, resyncPeriod time.Duration) *WatchRegistry {
	return &WatchRegistry{
		watches:      make(map[WatchKey]*WatchEntry),
		consumerKeys: make(map[string]map[WatchKey]struct{}),
		clients:      provider,
		resyncPeriod: resyncPeriod,
	}
}

// extractObject unwraps an object from the informer event, handling tombstones.
func extractObject(obj interface{}) *unstructured.Unstructured {
	if u, ok := obj.(*unstructured.Unstructured); ok {
		return u
	}
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		if u, ok := tombstone.Obj.(*unstructured.Unstructured); ok {
			return u
		}
	}
	return nil
}

// callbackToHandler adapts an EventCallback to a cache.ResourceEventHandler.
// Only registers closures for non-nil callback fields.
func callbackToHandler(cb EventCallback) cache.ResourceEventHandler {
	h := cache.ResourceEventHandlerFuncs{}

	if cb.OnAdd != nil {
		h.AddFunc = func(obj interface{}) {
			if u := extractObject(obj); u != nil {
				cb.OnAdd(u)
			}
		}
	}

	if cb.OnUpdate != nil {
		h.UpdateFunc = func(oldObj, newObj interface{}) {
			oldU := extractObject(oldObj)
			newU := extractObject(newObj)
			if oldU != nil && newU != nil {
				cb.OnUpdate(oldU, newU)
			}
		}
	}

	if cb.OnDelete != nil {
		h.DeleteFunc = func(obj interface{}) {
			if u := extractObject(obj); u != nil {
				cb.OnDelete(u)
			}
		}
	}

	return h
}

// EnsureWatchSet takes the complete set of watches a consumer currently needs and
// reconciles the registry to match. Watches that are no longer in the set are released
// automatically. New watches start informers as needed. Existing watches are returned as-is.
//
// parentCtx must outlive the consumer's individual operations (typically the manager context).
//
// Callbacks in each WatchRequest must not capture call-site-scoped state. See EventCallback.
//
// Calling with an empty or nil slice is equivalent to ReleaseAll.
func (r *WatchRegistry) EnsureWatchSet(
	parentCtx context.Context,
	consumerID string,
	watches []WatchRequest,
) ([]*WatchRegistration, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Empty set = release everything for this consumer
	if len(watches) == 0 {
		return nil, r.releaseAllLocked(consumerID)
	}

	// Build desired set
	desired := make(map[WatchKey]struct{}, len(watches))
	for _, w := range watches {
		desired[w.Key] = struct{}{}
	}

	// Collect stale keys before making changes. Stale removal is deferred
	// until after all new handlers succeed, so that on error the consumer
	// retains its previous watch set instead of ending up partially reconciled.
	var stale []WatchKey
	if current, ok := r.consumerKeys[consumerID]; ok {
		for key := range current {
			if _, want := desired[key]; !want {
				stale = append(stale, key)
			}
		}
	}

	// Ensure desired watches — add new handlers first
	var regs []*WatchRegistration
	var newlyAdded []WatchKey // keys where AddEventHandler was called (for rollback)
	newCurrent := make(map[WatchKey]struct{}, len(watches))

	for _, w := range watches {
		key := w.Key
		newCurrent[key] = struct{}{}

		entry, exists := r.watches[key]
		if !exists {
			cli, err := r.clients.ClientFor(key.Identity)
			if err == nil && cli == nil {
				// The provider is caller-supplied; guard against (nil, nil) so
				// the failure surfaces here instead of a panic inside the informer.
				err = fmt.Errorf("ClientFor returned a nil client")
			}
			if err != nil {
				return nil, r.rollbackNewlyAddedLocked(consumerID, newlyAdded,
					fmt.Errorf("failed to get client for %v: %w", key, err))
			}

			ctx, cancel := context.WithCancel(parentCtx)

			factory := kubedynamicinformer.NewFilteredDynamicSharedInformerFactory(
				cli,
				r.resyncPeriod,
				key.Namespace,
				nil,
			)

			informer := factory.ForResource(key.GVR).Informer()
			go informer.Run(ctx.Done())

			entry = &WatchEntry{
				cancel:   cancel,
				informer: informer,
				handlers: make(map[string]*handlerRegistration),
			}
			r.watches[key] = entry
		}

		// Deduplicate: already registered for this consumer
		if _, ok := entry.handlers[consumerID]; ok {
			regs = append(regs, &WatchRegistration{
				Key:        key,
				ConsumerID: consumerID,
				HasSynced:  entry.informer.HasSynced,
			})
			continue
		}

		reg, err := entry.informer.AddEventHandler(callbackToHandler(w.Callback))
		if err != nil {
			return nil, r.rollbackNewlyAddedLocked(consumerID, newlyAdded,
				fmt.Errorf("failed to add event handler for %v: %w", key, err))
		}

		entry.handlers[consumerID] = &handlerRegistration{
			registration: reg,
		}
		newlyAdded = append(newlyAdded, key)

		regs = append(regs, &WatchRegistration{
			Key:        key,
			ConsumerID: consumerID,
			HasSynced:  entry.informer.HasSynced,
		})
	}

	// All new handlers succeeded — now remove stale watches
	var errs []error
	for _, key := range stale {
		if err := r.releaseWatchLocked(key, consumerID); err != nil {
			errs = append(errs, err)
		}
	}

	r.consumerKeys[consumerID] = newCurrent

	return regs, errors.Join(errs...)
}

// rollbackNewlyAddedLocked releases the watches added earlier in the same
// EnsureWatchSet call and joins any release errors onto cause. Deduped
// (pre-existing) handlers are untouched, so the consumer keeps its previous
// watch set on failure. Caller must hold r.mu.
func (r *WatchRegistry) rollbackNewlyAddedLocked(consumerID string, newlyAdded []WatchKey, cause error) error {
	errs := []error{cause}
	for _, key := range newlyAdded {
		if rErr := r.releaseWatchLocked(key, consumerID); rErr != nil {
			errs = append(errs, rErr)
		}
	}
	return errors.Join(errs...)
}

// ReleaseAll releases all registrations for a given consumer across all watch keys.
// Equivalent to calling EnsureWatchSet with an empty slice.
// Idempotent - calling on an unknown or already-released consumer returns nil.
func (r *WatchRegistry) ReleaseAll(consumerID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.releaseAllLocked(consumerID)
}

func (r *WatchRegistry) releaseAllLocked(consumerID string) error {
	keys, ok := r.consumerKeys[consumerID]
	if !ok {
		return nil // already clean - idempotent
	}

	// Copy keys - releaseWatchLocked mutates consumerKeys
	toRelease := make([]WatchKey, 0, len(keys))
	for key := range keys {
		toRelease = append(toRelease, key)
	}

	var errs []error
	for _, key := range toRelease {
		if err := r.releaseWatchLocked(key, consumerID); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// releaseWatchLocked removes a single consumer's handler from a watch.
// If the watch has no remaining handlers, the informer is stopped and the watch is removed.
// Missing entries are treated as no-ops (returns nil) since this method is called from
// rollback and cleanup paths where partial state is expected.
// Callers that iterate consumerKeys must copy the key set first, since this method mutates it.
func (r *WatchRegistry) releaseWatchLocked(key WatchKey, consumerID string) error {
	entry, ok := r.watches[key]
	if !ok {
		return nil
	}

	hr, ok := entry.handlers[consumerID]
	if !ok {
		return nil
	}

	delete(entry.handlers, consumerID)

	// Keep consumerKeys in sync
	if keys, ok := r.consumerKeys[consumerID]; ok {
		delete(keys, key)
		if len(keys) == 0 {
			delete(r.consumerKeys, consumerID)
		}
	}

	var removeErr error
	if err := entry.informer.RemoveEventHandler(hr.registration); err != nil {
		removeErr = fmt.Errorf("failed to remove event handler (cleaned up anyway): %w", err)
	}

	if len(entry.handlers) == 0 {
		entry.cancel()
		delete(r.watches, key)
	}

	return removeErr
}
