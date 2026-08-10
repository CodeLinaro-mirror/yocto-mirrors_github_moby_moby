package extensions

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/containerd/log"
)

// Policy is how a point calls its providers: how long each provider gets, what
// a provider error means for the operation as a whole, and how that error
// reads. A point declares it once, as a value, instead of re-implementing it in
// every helper -- so the failure semantics a point promises in its doc comment
// are the ones [Each] and [Fold] actually enforce.
type Policy struct {
	// Timeout bounds each provider call independently. Zero leaves the calls
	// bounded only by the caller's context.
	//
	// It is enforced for out-of-process providers, which receive the deadline
	// over gRPC. An in-process provider is passed the same context but only
	// trusted to honor it: abandoning a direct Go call would leave the provider
	// mutating shared engine state after the caller moved on. State a
	// fail-closed point's guarantee accordingly.
	Timeout time.Duration

	// FailOpen continues with the remaining providers when one returns an error,
	// logging it, rather than failing the operation. A veto point -- anything
	// where a provider's error is meant to stop the operation -- must leave it
	// false.
	FailOpen bool

	// Action names what the provider's error did to the operation, for the
	// wrapped error: `<point> provider "<id>" <action>: <err>`. Leave it empty
	// for a plain failure.
	Action string
}

// wrap attributes err to the extension that produced it. Attribution is the
// whole point: an error surfacing from a fan-out is useless to an operator who
// cannot tell which of several installed extensions produced it.
func (p Policy) wrap(point PointID, extension ExtensionID, err error) error {
	if p.Action == "" {
		return fmt.Errorf("%s provider %q: %w", point.Name(), extension, err)
	}
	return fmt.Errorf("%s provider %q %s: %w", point.Name(), extension, p.Action, err)
}

// Each calls fn once per provider of p, in turn, under policy. It is the shape
// a point uses when the providers do not compose a value -- a validation or
// veto pass, or a notification.
//
// Fan-out order is unspecified. A point that needs a defined order must define
// it (see the guidance in the extensions authoring guide).
func Each[T any](ctx context.Context, p Point[T], r Resolver, policy Policy, fn func(context.Context, T) error) error {
	_, err := Fold(ctx, p, r, policy, struct{}{}, func(ctx context.Context, impl T, _ struct{}) (struct{}, error) {
		return struct{}{}, fn(ctx, impl)
	})
	return err
}

// Fold threads acc through every provider of p in turn and returns the final
// value, under policy. It is the shape a point uses when providers compose a
// value in sequence -- each seeing the value as shaped by the providers before
// it -- rather than answering independently.
//
// A provider that fails under a fail-closed policy aborts the fold and its
// partial value is discarded; under a fail-open one its contribution is dropped
// and the fold continues from the value it was given.
func Fold[T, A any](ctx context.Context, p Point[T], r Resolver, policy Policy, acc A, fn func(context.Context, T, A) (A, error)) (A, error) {
	providers, err := p.All(r)
	if err != nil {
		return acc, err
	}
	for _, provider := range providers {
		next, err := callProvider(ctx, p.id, policy, provider, acc, fn)
		if err != nil {
			if !policy.FailOpen {
				// Discard the partial value rather than hand back a half-applied
				// one: for a composing point such as the create-spec hook that
				// would be an OCI spec carrying some providers' changes but not
				// the rest, which is worse than no answer.
				var zero A
				return zero, err
			}
			log.G(ctx).WithError(err).Warn("extensions: skipping failed provider")
			continue
		}
		acc = next
	}
	return acc, nil
}

// callProvider bounds one provider call and attributes its error.
//
// It is deliberately a function per provider rather than the body of the loop
// above: that is what lets the deadline be released with defer on every path
// while still being a fresh budget per provider. Written inline, the same code
// invites the two mistakes that silently weaken a fail-closed point --
// `defer cancel()` inside the loop (holding every timer until the fan-out ends)
// and hoisting the context above it (one shared budget for all providers, so a
// slow first provider eats the rest).
func callProvider[T, A any](ctx context.Context, point PointID, policy Policy, provider TypedProvider[T], acc A, fn func(context.Context, T, A) (A, error)) (A, error) {
	if policy.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, policy.Timeout)
		defer cancel()
	}
	next, err := fn(ctx, provider.Impl, acc)
	if err != nil {
		return acc, policy.wrap(point, provider.Extension, err)
	}
	return next, nil
}

// Decide calls the point's single provider and returns its answer. It is the
// single-provider counterpart to [Each] and [Fold]: the shape for a point where
// one deciding voice answers -- with origin precedence, the one-provider limit,
// the per-call deadline, and error attribution implemented here rather than
// restated by every such point.
//
// When the provider fails -- an error from fn, which includes any validation
// the point layers over the raw call -- and the point has a masked built-in
// provider (the stock behavior an installed provider replaced), Decide
// degrades to it: like [io.Writer], it then returns the built-in's answer
// alongside the provider's error, so the caller applies the decision and
// surfaces the failure as it sees fit. A zero answer with a non-nil error
// means the point could not answer at all: no provider, more than one, or the
// failing provider was the built-in itself.
func Decide[T, R any](ctx context.Context, p Point[T], r Resolver, policy Policy, fn func(context.Context, T) (R, error)) (R, error) {
	var zero R
	all := r.Providers(p.id)
	effective := EffectiveProviders(all)
	switch len(effective) {
	case 1:
	case 0:
		return zero, fmt.Errorf("point %q has no providers", p.id)
	default:
		return zero, fmt.Errorf("point %q has multiple providers", p.id)
	}
	primary, err := typedDecider[T](p.id, effective[0])
	if err != nil {
		return zero, err
	}
	res, perr := decideCall(ctx, p.id, policy, primary, fn)
	if perr == nil {
		return res, nil
	}

	// Degrade to the masked built-in, if the failing provider was not the
	// built-in itself (nothing else installed): there is no second opinion to
	// ask then. The lookup stays internal to Decide on purpose -- exposing the
	// built-in to callers would invite exactly the by-hand fallback selection
	// this combinator exists to end.
	for _, candidate := range all {
		if !candidate.Builtin || candidate.Extension == primary.Extension {
			continue
		}
		def, err := typedDecider[T](p.id, candidate)
		if err != nil {
			return zero, errors.Join(perr, err)
		}
		res, ferr := decideCall(ctx, p.id, policy, def, fn)
		if ferr != nil {
			return zero, errors.Join(perr, ferr)
		}
		return res, perr
	}
	return zero, perr
}

// typedDecider converts one enumerated provider to the point's typed handle.
func typedDecider[T any](point PointID, provider ResolvedProvider) (TypedProvider[T], error) {
	impl, err := typedProvider[T](point, provider.Extension, provider.Impl)
	if err != nil {
		return TypedProvider[T]{}, err
	}
	return TypedProvider[T]{Extension: provider.Extension, Impl: impl}, nil
}

// decideCall bounds one provider call under policy and attributes its error,
// discarding any partial answer: an errored call must never contribute a value
// (see [callProvider] for why this is a function rather than inline).
func decideCall[T, R any](ctx context.Context, point PointID, policy Policy, provider TypedProvider[T], fn func(context.Context, T) (R, error)) (R, error) {
	if policy.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, policy.Timeout)
		defer cancel()
	}
	res, err := fn(ctx, provider.Impl)
	if err != nil {
		var zero R
		return zero, policy.wrap(point, provider.Extension, err)
	}
	return res, nil
}
