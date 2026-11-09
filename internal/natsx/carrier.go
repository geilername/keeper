package natsx

import (
	"net/textproto"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel/propagation"
)

// headerCarrier writes trace headers under their exact lowercase W3C names
// ("traceparent") and reads either spelling. nats.Header.Set would
// canonicalise to "Traceparent", which the Python scorer's case-sensitive
// propagator never finds, and nats.Header.Get misses the lowercase key that
// Python writes.
type headerCarrier nats.Header

var _ propagation.TextMapCarrier = headerCarrier{}

func (c headerCarrier) Get(key string) string {
	if v := c[key]; len(v) > 0 {
		return v[0]
	}
	if v := c[textproto.CanonicalMIMEHeaderKey(key)]; len(v) > 0 {
		return v[0]
	}
	return ""
}

func (c headerCarrier) Set(key, value string) { c[key] = []string{value} }

func (c headerCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	return keys
}
