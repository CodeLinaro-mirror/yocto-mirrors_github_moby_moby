// Package extensionstest provides test doubles for the extension framework, so
// a point's tests can drive its helpers without standing up a broker or a host.
package extensionstest

import (
	"fmt"

	"github.com/moby/moby/v2/internal/extensions"
)

// Resolver is an [extensions.Resolver] answering from a fixed set of providers,
// for testing a point's fan-out helpers. It serves the same providers for every
// point id: a resolver in a test stands in for one point, so the id is not worth
// keying on.
type Resolver []extensions.ResolvedProvider

// Provide returns a Resolver serving impls in order, under generated extension
// ids. Construct a [Resolver] literal instead when a test asserts on the ids.
func Provide(impls ...any) Resolver {
	r := make(Resolver, 0, len(impls))
	for i, impl := range impls {
		r = append(r, extensions.ResolvedProvider{
			Extension: extensions.ExtensionID(fmt.Sprintf("org.example.test%d.v1", i+1)),
			Impl:      impl,
		})
	}
	return r
}

// Provider implements [extensions.Resolver].
func (r Resolver) Provider(_ extensions.PointID, id extensions.ExtensionID) (any, error) {
	for _, p := range r {
		if p.Extension == id {
			return p.Impl, nil
		}
	}
	return nil, fmt.Errorf("no provider for extension %q", id)
}

// Providers implements [extensions.Resolver].
func (r Resolver) Providers(extensions.PointID) []extensions.ResolvedProvider { return r }
