package extensions

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// ExtensionID identifies a deployable extension.
type ExtensionID string

// PointID identifies an extension point contract.
type PointID string

// Name is the point's short name: the segment before its version, e.g.
// "create_spec" for org.mobyproject.extension.container.create_spec.v0. It is
// what a provider error is attributed to, because the full id is too long to
// read in a message an operator sees. The shape is guaranteed by
// [pointIDPattern], so a well-formed id always has one.
func (id PointID) Name() string {
	segments := strings.Split(string(id), ".")
	if len(segments) < 2 {
		return string(id)
	}
	return segments[len(segments)-2]
}

// pointIDPattern is the required shape of a point id: a reverse-DNS-style,
// dot-separated name of at least three segments ending in a version, i.e.
// <tld>.<name>...vN (e.g. org.mobyproject.extension.volume.driver.v1). Segments
// are lowercase and may contain digits, hyphens, and underscores; the last is a
// version like v0 or v12.
var pointIDPattern = lazyRegexp(`^[a-z][a-z0-9]*(\.[a-z0-9_-]+)+\.v[0-9]+$`)

// extensionIDPattern is the required shape of an extension id: a reverse-DNS
// name of at least two lowercase, dot-separated segments followed by a
// mandatory version segment (e.g. org.example.no-privileged.v1 or
// com.docker.compose.v1). Segments are lowercase alphanumerics and may contain
// hyphens but not lead or trail with one; the final segment is a version like v0
// or v12. That version is a namespace element, not a semantic version, and is
// distinct from the versions of the points the extension implements: migrating
// com.foo.v1 to com.foo.v2 is a new extension -- a different id, binary, and
// config -- not an upgrade in place; the two can coexist during migration.
// Because an id is also a config key, a dependency name, and the on-disk binary
// name, this shape doubles as a safety rule: it admits no path separators, no
// "..", no uppercase, and no other path- or shell-hostile characters.
var extensionIDPattern = lazyRegexp(`^[a-z0-9]+(-[a-z0-9]+)*(\.[a-z0-9]+(-[a-z0-9]+)*)+\.v[0-9]+$`)

// ValidateExtensionID reports whether id is a well-formed extension id (see
// [extensionIDPattern]). It is enforced where an extension is registered, so a
// malformed id -- in-process or delivered by a launched binary -- is rejected
// rather than used as a binary name or config key.
func ValidateExtensionID(id ExtensionID) error {
	if id == "" {
		return errors.New("extension id is required")
	}
	if !extensionIDPattern().MatchString(string(id)) {
		return fmt.Errorf("invalid extension id %q: want a versioned reverse-DNS name like org.example.myext.v1, lowercase, no path-hostile characters", id)
	}
	return nil
}

// lazyRegexp returns a regexp accessor that compiles pattern on first use.
// The daemon/internal/lazyregexp helper is not importable from this package.
func lazyRegexp(pattern string) func() *regexp.Regexp {
	return sync.OnceValue(func() *regexp.Regexp {
		re, err := regexp.Compile(pattern)
		if err != nil {
			panic(err)
		}
		return re
	})
}

// Dependency declares one extension dependency.
type Dependency struct {
	Point     PointID
	Extension ExtensionID
	Optional  bool
}

// Provider is an extension's in-process implementation of one point, as
// declared. Impl stores the implementation behind typed point handles. It
// carries no extension id: which extension declared it is known from the
// declaration it belongs to, and the broker reports it as the
// [ResolvedProvider.Extension] of a lookup result.
type Provider struct {
	Point PointID
	Impl  any
}

// ResolvedProvider is a provider as returned from a lookup: the same
// implementation, plus the id of the extension that provides it and whether
// that extension is a built-in. The registrar records the origin -- a built-in
// is registered by the host's own program, an installed extension arrives from
// the outside -- so no declaration ever claims it. It is the output
// counterpart to the declaration-side [Provider].
type ResolvedProvider struct {
	Extension ExtensionID
	Impl      any
	Builtin   bool
}

// EffectiveProviders applies origin precedence for single-provider resolution:
// built-in providers -- the behavior a host ships out of the box -- stand in
// only while the point has no installed provider, and yield as soon as an
// extension is installed for the point. That is what makes a single-provider
// point replaceable without any disable knob or declaration: the default is,
// definitionally, what came built in. Two installed providers still conflict.
//
// Only single-provider resolution ([Point.Single], [Decide], a host's
// startup checks) applies it; fan-out accessors enumerate everyone, so a
// built-in provider of a fan-out point runs beside installed ones rather than
// being replaced by them. A by-id lookup ([Point.ByExtension]) also ignores
// it, since naming an extension explicitly outranks defaulting.
func EffectiveProviders(providers []ResolvedProvider) []ResolvedProvider {
	var installed, builtins []ResolvedProvider
	for _, p := range providers {
		if p.Builtin {
			builtins = append(builtins, p)
		} else {
			installed = append(installed, p)
		}
	}
	if len(installed) == 0 {
		return builtins
	}
	return installed
}

// TypedProvider is a provider returned through a typed point handle.
type TypedProvider[T any] struct {
	Extension ExtensionID
	Impl      T
}

// Point binds a point ID to the Go interface implemented by its providers.
type Point[T any] struct {
	id PointID
}

// DefinePoint binds id to provider interface T. It panics if id is not a valid
// point id (see [pointIDPattern]): a namespaced, versioned name such as
// org.mobyproject.extension.volume.driver.v1. The id is fixed in source, so a
// malformed one is a programming error worth catching at startup rather than
// when some extension first names the point -- the same rationale as compiling
// a constant regexp up front.
func DefinePoint[T any](id PointID) Point[T] {
	if !pointIDPattern().MatchString(string(id)) {
		panic(fmt.Sprintf("extensions: invalid point id %q: want <tld>.<name>...vN, e.g. org.mobyproject.extension.volume.driver.v1", id))
	}
	return Point[T]{id: id}
}

// DefineSinglePoint is [DefinePoint] for a point that admits one deciding
// provider rather than a fan-out -- the engine resolves it with [Point.Single]
// or [Decide]. The cardinality is part of the contract, so it is declared here,
// in the contract's source: the wire generator reads which constructor the
// contract calls and carries the constraint into the point's generated
// ClientPoint, from which a host learns to reject an installation with two
// providers at startup (a default provider yields instead of counting).
func DefineSinglePoint[T any](id PointID) Point[T] {
	return DefinePoint[T](id)
}

// ID returns the point identifier.
func (p Point[T]) ID() PointID {
	return p.id
}

// Provide returns a provider declaration for impl.
func (p Point[T]) Provide(impl T) Provider {
	return Provider{Point: p.id, Impl: impl}
}

// Dependency returns a required dependency declaration for the point: at least
// one provider must exist before the dependent initializes.
func (p Point[T]) Dependency() Dependency {
	return Dependency{Point: p.id}
}

// OptionalDependency returns an optional dependency declaration for the point:
// the dependent still initializes, ordered after any providers, when none exist.
func (p Point[T]) OptionalDependency() Dependency {
	return Dependency{Point: p.id, Optional: true}
}

// ByExtension returns the point provider implemented by extension.
func (p Point[T]) ByExtension(r Resolver, extension ExtensionID) (T, error) {
	provider, err := r.Provider(p.id, extension)
	if err != nil {
		var zero T
		return zero, err
	}
	return typedProvider[T](p.id, extension, provider)
}

// Single returns the only point provider, after origin precedence
// ([EffectiveProviders]): a built-in provider does not count against the
// one-provider limit, it stands in when nothing is installed.
func (p Point[T]) Single(r Resolver) (T, error) {
	providers := EffectiveProviders(r.Providers(p.id))
	var zero T
	switch len(providers) {
	case 0:
		return zero, fmt.Errorf("point %q has no providers", p.id)
	case 1:
		return typedProvider[T](p.id, providers[0].Extension, providers[0].Impl)
	default:
		return zero, fmt.Errorf("point %q has multiple providers", p.id)
	}
}

// Enabled reports whether any extension provides the point. A caller uses it to
// skip building a request when nothing will receive it -- worth doing where the
// request is expensive to build, such as the create-spec hook marshaling a whole
// OCI spec on every container start.
//
// It cannot fail: a provider registered with the wrong type is reported where it
// is actually called, not here.
func (p Point[T]) Enabled(r Resolver) bool {
	return len(r.Providers(p.id)) > 0
}

// All returns all point providers, built-ins and installed alike: a fan-out
// point's built-in provider runs beside installed providers rather than being
// replaced by them -- origin precedence is single-provider resolution's rule
// (see [EffectiveProviders]).
func (p Point[T]) All(r Resolver) ([]TypedProvider[T], error) {
	providers := r.Providers(p.id)
	typed := make([]TypedProvider[T], 0, len(providers))
	for _, provider := range providers {
		impl, err := typedProvider[T](p.id, provider.Extension, provider.Impl)
		if err != nil {
			return nil, err
		}
		typed = append(typed, TypedProvider[T]{Extension: provider.Extension, Impl: impl})
	}
	return typed, nil
}

func typedProvider[T any](point PointID, extension ExtensionID, provider any) (T, error) {
	typed, ok := provider.(T)
	if ok {
		return typed, nil
	}
	var zero T
	if extension == "" {
		return zero, fmt.Errorf("point %q provider has type %T", point, provider)
	}
	return zero, fmt.Errorf("extension %q provider for point %q has type %T", extension, point, provider)
}

// Resolver exposes provider lookup to extension initializers. It only
// enumerates: which provider a caller gets -- default masking, the
// single-provider limit -- is decided by the typed [Point] accessors, so every
// resolver behaves identically.
type Resolver interface {
	Provider(PointID, ExtensionID) (any, error)
	Providers(PointID) []ResolvedProvider
}

// Registrar registers extensions.
type Registrar interface {
	Register(Extension) error
}

// RegisterAll registers exts with registrar.
func RegisterAll(registrar Registrar, exts ...Extension) error {
	for _, ext := range exts {
		if err := registrar.Register(ext); err != nil {
			return err
		}
	}
	return nil
}

// Config is an extension's per-extension configuration, delivered by id: to
// in-process extensions through Init, and to out-of-process ones through the
// startup handshake. It is the parsed configuration object (as from
// daemon.json), so an extension reads its keys directly or decodes it into a
// struct.
type Config = map[string]any

// Extension is something a host runs: it declares itself. A stateless extension
// is a [Declaration] wrapped with [New]; an extension that holds state
// implements this interface on its own type, so the object that implements the
// point also configures itself from its config in Init -- no package-level
// state needed.
type Extension interface {
	// Declaration returns the extension's id, providers, dependencies, and
	// conflicts, plus its optional Init and Shutdown.
	Declaration() Declaration
}

// Declaration declares one extension and its dependencies.
// Conflicts names extensions that cannot coexist with this extension. Init, if
// set, configures the extension from the config the host delivers; Shutdown
// tears it down.
type Declaration struct {
	ID           ExtensionID
	Providers    []Provider
	Dependencies []Dependency
	Conflicts    []ExtensionID
	Init         func(context.Context, Config, Resolver) error
	Shutdown     func(context.Context) error
}

// New wraps a static Declaration as an [Extension].
func New(d Declaration) Extension { return staticExtension{d} }

type staticExtension struct{ decl Declaration }

func (e staticExtension) Declaration() Declaration { return e.decl }
