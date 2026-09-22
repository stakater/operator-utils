package publisher

import (
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// capturingRegisterer registers collectors on the wrapped Registerer and
// remembers them, so the bridge can be told which metric families this
// module put into a shared registry.
//
// Not safe for concurrent use, unlike the prometheus.Registerer it
// implements. Only buildPrometheusReader registers on it, once, before
// the value escapes to anything else.
type capturingRegisterer struct {
	prometheus.Registerer
	captured []prometheus.Collector
}

func (r *capturingRegisterer) Register(c prometheus.Collector) error {
	if err := r.Registerer.Register(c); err != nil {
		return err
	}
	r.captured = append(r.captured, c)
	return nil
}

// MustRegister must be overridden too. The embedded Registerer would
// otherwise register without capturing, and the bridge would re-export
// those families. Unregister is deliberately left promoted: the OTel
// exporter never unregisters, and a stale entry in captured would only
// over-exclude.
func (r *capturingRegisterer) MustRegister(cs ...prometheus.Collector) {
	for _, c := range cs {
		if err := r.Register(c); err != nil {
			panic(err)
		}
	}
}

// exceptGatherer gathers base and drops every family that own also
// produces. It is how the OTLP bridge avoids re-exporting the SDK metrics
// the Prometheus exporter wrote into controller-runtime's registry.
//
// ponytail: own.Gather() aggregates our collector a second time per export
// to learn its family names. The OTel collector is unchecked, so Describe
// yields nothing and there is no cheaper way to ask. Cache the name set if
// export cost ever shows up in a profile.
type exceptGatherer struct{ base, own prometheus.Gatherer }

func (g exceptGatherer) Gather() ([]*dto.MetricFamily, error) {
	all, err := g.base.Gather()
	if err != nil {
		return nil, err
	}
	mine, err := g.own.Gather()
	if err != nil {
		return nil, err
	}
	skip := make(map[string]struct{}, len(mine))
	for _, f := range mine {
		skip[f.GetName()] = struct{}{}
	}
	out := make([]*dto.MetricFamily, 0, len(all))
	for _, f := range all {
		if _, ok := skip[f.GetName()]; !ok {
			out = append(out, f)
		}
	}
	return out, nil
}

// bridgeGathererFor returns what the OTLP bridge should read. The bridge
// always reads controller-runtime's registry; the only question is
// whether this module's own families are in there and have to be
// filtered out.
//
// Filtering happens only when the Prometheus reader was pointed at that
// exact registry. capreg is nil when no Prometheus exporter is running,
// and registerer is some other registry when the caller redirected the
// reader elsewhere; in both cases nothing of ours is in the shared
// registry and filtering would only drop controller-runtime families
// that happen to share a name.
//
// Wrapping controller-runtime's registry (prometheus.WrapRegistererWith,
// WrapRegistererWithPrefix) is not supported: the wrapper rewrites names
// on the way in, the mirror below cannot know how, and the filter would
// silently match nothing. See PrometheusConfig.Registerer.
func bridgeGathererFor(registerer prometheus.Registerer, capreg *capturingRegisterer) prometheus.Gatherer {
	if capreg == nil || registerer != prometheus.Registerer(ctrlmetrics.Registry) {
		return ctrlmetrics.Registry
	}
	own := prometheus.NewRegistry()
	for _, c := range capreg.captured {
		own.MustRegister(c)
	}
	return exceptGatherer{base: ctrlmetrics.Registry, own: own}
}
