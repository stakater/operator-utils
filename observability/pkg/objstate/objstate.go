// Package objstate publishes per-OBJECT state metrics with correct series
// lifecycle: a series exists exactly while its object does.
//
// Sync instruments cannot do this - once an attribute set is recorded, its
// last value is exported forever, so a deleted CostJob/Supervisor/
// TemplateInstance would keep its gauges (and keep its alerts firing)
// indefinitely. Every MTO operator therefore hand-rolled the same
// delete-with-the-CR machinery on raw Prometheus vectors. This package is
// that machinery, once, built the OTel-native way: observable instruments
// whose callback reports only the objects currently in the Set, so absence
// is automatic and there is nothing to delete by hand.
//
// A Set holds many objects of one kind, identified by a fixed tuple of
// attribute keys. Put replaces an object's whole state (stale enum values or
// condition reasons cannot linger - replacement is the hygiene), Delete drops
// every series of the object across all instruments, and counters survive Put
// (a per-reconcile state republish must never reset a reconcile counter).
package objstate

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/stakater/operator-utils/observability/pkg/naming"
)

// ID is an object's identity: one value per identity key, in the order the
// keys were given to NewSet.
type ID []string

// Set publishes the state of many objects of one kind.
type Set struct {
	meter  metric.Meter
	idKeys []string

	mu           sync.RWMutex
	names        map[string]struct{}
	values       []*Value
	timestamps   []*Timestamp
	enums        []*Enum
	conditions   []*Condition
	counters     []*Counter
	objects      map[string]*object
	registration metric.Registration
}

// Value is a plain per-object gauge.
type Value struct{ inst metric.Float64ObservableGauge }

// Timestamp is a per-object unix-seconds gauge, absent until first set - the
// lazy-registration semantics freshness alerts rely on (a job that never
// succeeded has no last-success series to age).
type Timestamp struct{ inst metric.Float64ObservableGauge }

// Enum is a one-hot gauge over a closed value set. Every object in the Set
// emits every enum value - exactly one 1 once set, all zeros before - so
// "tracked but never transitioned" stays expressible.
type Enum struct {
	inst   metric.Float64ObservableGauge
	key    string
	values []string
}

// Condition is a 0/1 gauge carrying its current reason as an attribute -
// exactly one reason series per object, absent until first set.
type Condition struct {
	inst metric.Float64ObservableGauge
	key  string
}

// Counter is a per-object monotonic counter (reconcile storms). It survives
// Put and is dropped only by Delete.
type Counter struct {
	inst metric.Float64ObservableCounter
}

type condVal struct {
	ok     bool
	reason string
}

type object struct {
	id         ID
	extraAttrs []attribute.KeyValue
	values     map[*Value]float64
	timestamps map[*Timestamp]float64
	enums      map[*Enum]string
	conditions map[*Condition]condVal
	counters   map[*Counter]float64
}

// NewSet creates a Set whose objects are identified by the given attribute
// keys (at least one; each validated like any attribute key).
func NewSet(meter metric.Meter, identityKeys ...string) (*Set, error) {
	if meter == nil {
		return nil, fmt.Errorf("objstate: meter must not be nil")
	}
	if len(identityKeys) == 0 {
		return nil, fmt.Errorf("objstate: at least one identity key is required")
	}
	for _, k := range identityKeys {
		if err := naming.ValidateAttributeKey(k); err != nil {
			return nil, fmt.Errorf("objstate: identity key %q: %w", k, err)
		}
	}
	return &Set{
		meter:   meter,
		idKeys:  identityKeys,
		names:   map[string]struct{}{},
		objects: map[string]*object{},
	}, nil
}

func (s *Set) reserve(name string) error {
	if err := naming.ValidateMetricName(name); err != nil {
		return err
	}
	if _, dup := s.names[name]; dup {
		return fmt.Errorf("objstate: metric %q already registered on this Set", name)
	}
	s.names[name] = struct{}{}
	return nil
}

// reregister (re)installs the single callback that serves every instrument of
// the Set, so all series of an object come from one consistent snapshot.
func (s *Set) reregister() error {
	if s.registration != nil {
		if err := s.registration.Unregister(); err != nil {
			return err
		}
		s.registration = nil
	}
	obs := make([]metric.Observable, 0,
		len(s.values)+len(s.timestamps)+len(s.enums)+len(s.conditions)+len(s.counters))
	for _, i := range s.values {
		obs = append(obs, i.inst)
	}
	for _, i := range s.timestamps {
		obs = append(obs, i.inst)
	}
	for _, i := range s.enums {
		obs = append(obs, i.inst)
	}
	for _, i := range s.conditions {
		obs = append(obs, i.inst)
	}
	for _, i := range s.counters {
		obs = append(obs, i.inst)
	}
	reg, err := s.meter.RegisterCallback(s.observe, obs...)
	if err != nil {
		return err
	}
	s.registration = reg
	return nil
}

// Value registers a plain per-object gauge.
func (s *Set) Value(name, description string) (*Value, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reserve(name); err != nil {
		return nil, err
	}
	inst, err := s.meter.Float64ObservableGauge(name, metric.WithDescription(description))
	if err != nil {
		return nil, err
	}
	v := &Value{inst: inst}
	s.values = append(s.values, v)
	return v, s.reregister()
}

// Timestamp registers a per-object unix-seconds gauge, absent until set.
func (s *Set) Timestamp(name, description string) (*Timestamp, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reserve(name); err != nil {
		return nil, err
	}
	inst, err := s.meter.Float64ObservableGauge(name, metric.WithDescription(description))
	if err != nil {
		return nil, err
	}
	t := &Timestamp{inst: inst}
	s.timestamps = append(s.timestamps, t)
	return t, s.reregister()
}

// Enum registers a one-hot gauge over the closed value set, keyed by the given
// attribute. Every object emits every value: one 1 once set, all zeros before.
func (s *Set) Enum(name, description, key string, values ...string) (*Enum, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := naming.ValidateAttributeKey(key); err != nil {
		return nil, fmt.Errorf("objstate: enum key %q: %w", key, err)
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("objstate: enum %q needs at least one value", name)
	}
	if err := s.reserve(name); err != nil {
		return nil, err
	}
	inst, err := s.meter.Float64ObservableGauge(name, metric.WithDescription(description))
	if err != nil {
		return nil, err
	}
	e := &Enum{inst: inst, key: key, values: append([]string(nil), values...)}
	s.enums = append(s.enums, e)
	return e, s.reregister()
}

// Condition registers a 0/1 gauge carrying its current reason under the given
// attribute key - one reason series per object, absent until set.
func (s *Set) Condition(name, description, reasonKey string) (*Condition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := naming.ValidateAttributeKey(reasonKey); err != nil {
		return nil, fmt.Errorf("objstate: condition reason key %q: %w", reasonKey, err)
	}
	if err := s.reserve(name); err != nil {
		return nil, err
	}
	inst, err := s.meter.Float64ObservableGauge(name, metric.WithDescription(description))
	if err != nil {
		return nil, err
	}
	c := &Condition{inst: inst, key: reasonKey}
	s.conditions = append(s.conditions, c)
	return c, s.reregister()
}

// Counter registers a per-object monotonic counter. It survives Put and is
// dropped only when the object is.
func (s *Set) Counter(name, description string) (*Counter, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reserve(name); err != nil {
		return nil, err
	}
	inst, err := s.meter.Float64ObservableCounter(name, metric.WithDescription(description))
	if err != nil {
		return nil, err
	}
	c := &Counter{inst: inst}
	s.counters = append(s.counters, c)
	return c, s.reregister()
}

// State is the write surface inside Put.
type State struct{ o *object }

// Value sets a plain gauge for the object.
func (st *State) Value(v *Value, val float64) { st.o.values[v] = val }

// Timestamp sets a timestamp gauge for the object.
func (st *State) Timestamp(t *Timestamp, at time.Time) {
	st.o.timestamps[t] = float64(at.Unix())
}

// Enum sets the object's current enum value. An unknown value is recorded
// verbatim and simply matches no one-hot series - the closed set stays closed.
func (st *State) Enum(e *Enum, value string) { st.o.enums[e] = value }

// Condition sets the object's condition status and current reason.
func (st *State) Condition(c *Condition, ok bool, reason string) {
	st.o.conditions[c] = condVal{ok: ok, reason: reason}
}

// Put replaces the object's state: everything not set inside fn is gone after
// (stale enum values or condition reasons cannot linger), except counters,
// which survive. extraAttrs are constant per-object attributes applied to
// every series of the object (a CostJob's type, a supervisor's kind).
func (s *Set) Put(id ID, extraAttrs []attribute.KeyValue, fn func(*State)) error {
	key, err := s.keyFor(id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	fresh := &object{
		id:         append(ID(nil), id...),
		extraAttrs: append([]attribute.KeyValue(nil), extraAttrs...),
		values:     map[*Value]float64{},
		timestamps: map[*Timestamp]float64{},
		enums:      map[*Enum]string{},
		conditions: map[*Condition]condVal{},
		counters:   map[*Counter]float64{},
	}
	if prev, ok := s.objects[key]; ok {
		fresh.counters = prev.counters
	}
	if fn != nil {
		fn(&State{o: fresh})
	}
	s.objects[key] = fresh
	return nil
}

// Add increments the object's counter, creating the object (with identity
// attributes only) if it is not tracked yet - a deletion-path reconcile may
// count before any Put.
func (s *Set) Add(c *Counter, id ID, delta float64) error {
	if delta < 0 {
		return fmt.Errorf("objstate: counter delta must be non-negative, got %v", delta)
	}
	key, err := s.keyFor(id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objects[key]
	if !ok {
		o = &object{
			id:         append(ID(nil), id...),
			values:     map[*Value]float64{},
			timestamps: map[*Timestamp]float64{},
			enums:      map[*Enum]string{},
			conditions: map[*Condition]condVal{},
			counters:   map[*Counter]float64{},
		}
		s.objects[key] = o
	}
	o.counters[c] += delta
	return nil
}

// Inc is Add with a delta of 1.
func (s *Set) Inc(c *Counter, id ID) error { return s.Add(c, id, 1) }

// Delete drops the object: every series of every instrument vanishes at the
// next collection. Deleting an untracked object is a no-op.
func (s *Set) Delete(id ID) error {
	key, err := s.keyFor(id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, key)
	return nil
}

func (s *Set) keyFor(id ID) (string, error) {
	if len(id) != len(s.idKeys) {
		return "", fmt.Errorf("objstate: identity needs %d value(s) (%v), got %d",
			len(s.idKeys), s.idKeys, len(id))
	}
	key := ""
	for _, v := range id {
		key += v + "\x00"
	}
	return key, nil
}

func (s *Set) baseAttrs(o *object) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, len(s.idKeys)+len(o.extraAttrs))
	for i, k := range s.idKeys {
		attrs = append(attrs, attribute.String(k, o.id[i]))
	}
	return append(attrs, o.extraAttrs...)
}

// observe serves every instrument from one consistent snapshot of the Set.
func (s *Set) observe(_ context.Context, obs metric.Observer) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, o := range s.objects {
		base := s.baseAttrs(o)
		for inst, v := range o.values {
			obs.ObserveFloat64(inst.inst, v, metric.WithAttributes(base...))
		}
		for inst, v := range o.timestamps {
			obs.ObserveFloat64(inst.inst, v, metric.WithAttributes(base...))
		}
		for _, e := range s.enums {
			current := o.enums[e] // "" when never set: all zeros, the presence signal
			for _, val := range e.values {
				f := 0.0
				if current == val {
					f = 1.0
				}
				attrs := append(append([]attribute.KeyValue(nil), base...), attribute.String(e.key, val))
				obs.ObserveFloat64(e.inst, f, metric.WithAttributes(attrs...))
			}
		}
		for inst, cv := range o.conditions {
			f := 0.0
			if cv.ok {
				f = 1.0
			}
			attrs := append(append([]attribute.KeyValue(nil), base...), attribute.String(inst.key, cv.reason))
			obs.ObserveFloat64(inst.inst, f, metric.WithAttributes(attrs...))
		}
		for inst, v := range o.counters {
			obs.ObserveFloat64(inst.inst, v, metric.WithAttributes(base...))
		}
	}
	return nil
}
