package extensions

import (
	"context"
	"errors"
	"testing"
	"time"

	"gotest.tools/v3/assert"
	"gotest.tools/v3/assert/cmp"
)

// TestEachBoundsEveryProviderIndependently is the regression this whole
// combinator exists for. Written out by hand, the fan-out loop invites two
// mistakes that no existing test would catch: hoisting the timeout above the
// loop, which gives all providers one shared budget, and deferring the cancel
// inside it, which holds every timer open. Either way a slow provider silently
// shortens the deadline of the providers after it, weakening a fail-closed
// point exactly where it is load-bearing.
//
// Each provider records the budget it was given; the second must see a full one
// even though the first consumed most of its own.
func TestEachBoundsEveryProviderIndependently(t *testing.T) {
	const timeout = 200 * time.Millisecond
	var budgets []time.Duration
	record := callerFunc(func(ctx context.Context) error {
		deadline, ok := ctx.Deadline()
		assert.Assert(t, ok, "provider must receive a deadline")
		budgets = append(budgets, time.Until(deadline))
		return nil
	})
	slow := callerFunc(func(ctx context.Context) error {
		deadline, ok := ctx.Deadline()
		assert.Assert(t, ok, "provider must receive a deadline")
		budgets = append(budgets, time.Until(deadline))
		// Burn most of this provider's budget without failing.
		time.Sleep(timeout / 2)
		return nil
	})

	err := Each(context.Background(), testPoint, resolverOf(
		ResolvedProvider{Extension: "slow", Impl: slow},
		ResolvedProvider{Extension: "second", Impl: record},
	), Policy{Timeout: timeout}, func(ctx context.Context, c caller) error {
		return c.Call(ctx)
	})
	assert.NilError(t, err)

	assert.Assert(t, cmp.Len(budgets, 2))
	// The second provider gets its own full budget, not what the first left.
	assert.Assert(t, budgets[1] > timeout/2,
		"second provider got %s of a %s budget; the deadline is shared, not per-provider", budgets[1], timeout)
}

// TestEachAbortsOnErrorAndAttributes locks down fail-closed semantics: the first
// failure stops the fan-out, and the error names the extension that caused it --
// without which an operator running several extensions cannot tell which one
// vetoed.
func TestEachAbortsOnErrorAndAttributes(t *testing.T) {
	called := 0
	count := callerFunc(func(context.Context) error { called++; return nil })
	veto := callerFunc(func(context.Context) error { called++; return errors.New("not allowed") })

	err := Each(context.Background(), testPoint, resolverOf(
		ResolvedProvider{Extension: "org.example.veto.v1", Impl: veto},
		ResolvedProvider{Extension: "org.example.after.v1", Impl: count},
	), Policy{Action: "vetoed the start"}, func(ctx context.Context, c caller) error {
		return c.Call(ctx)
	})

	assert.ErrorContains(t, err, `test provider "org.example.veto.v1" vetoed the start: not allowed`)
	assert.Equal(t, called, 1, "a fail-closed point must not call providers after a veto")
}

// TestEachFailOpenSkipsAndContinues is the other half of the policy: a fail-open
// point drops the failing provider's contribution rather than the operation.
func TestEachFailOpenSkipsAndContinues(t *testing.T) {
	called := 0
	count := callerFunc(func(context.Context) error { called++; return nil })
	boom := callerFunc(func(context.Context) error { called++; return errors.New("boom") })

	err := Each(context.Background(), testPoint, resolverOf(
		ResolvedProvider{Extension: "org.example.broken.v1", Impl: boom},
		ResolvedProvider{Extension: "org.example.ok.v1", Impl: count},
	), Policy{FailOpen: true}, func(ctx context.Context, c caller) error {
		return c.Call(ctx)
	})

	assert.NilError(t, err)
	assert.Equal(t, called, 2, "a fail-open point must continue past a failing provider")
}

// TestFoldThreadsValueInOrder checks the sequential-composition shape: each
// provider sees the value as shaped by the ones before it.
func TestFoldThreadsValueInOrder(t *testing.T) {
	noop := callerFunc(func(context.Context) error { return nil })
	out, err := Fold(context.Background(), testPoint, resolverOf(
		ResolvedProvider{Extension: "a", Impl: noop},
		ResolvedProvider{Extension: "b", Impl: noop},
	), Policy{}, "seed", func(_ context.Context, _ caller, acc string) (string, error) {
		return acc + "+", nil
	})
	assert.NilError(t, err)
	assert.Equal(t, out, "seed++")
}

// TestFoldDiscardsPartialValueOnError guards the fail-closed contract for a
// composing point: a veto must not leave the caller holding a half-applied
// value, which for the create-spec hook would be a partially adjusted OCI spec.
func TestFoldDiscardsPartialValueOnError(t *testing.T) {
	out, err := Fold(context.Background(), testPoint, resolverOf(
		ResolvedProvider{Extension: "a", Impl: callerFunc(func(context.Context) error { return nil })},
		ResolvedProvider{Extension: "b", Impl: callerFunc(func(context.Context) error { return errors.New("no") })},
	), Policy{}, "seed", func(_ context.Context, c caller, acc string) (string, error) {
		if err := c.Call(context.Background()); err != nil {
			return acc, err
		}
		return acc + "+", nil
	})
	assert.ErrorContains(t, err, "no")
	assert.Equal(t, out, "", "a fail-closed fold must discard the partial value")
}

// TestPointIDName checks the short name a provider error is attributed to.
func TestPointIDName(t *testing.T) {
	for _, tc := range []struct {
		id   PointID
		want string
	}{
		{"org.mobyproject.extension.container.create_spec.v0", "create_spec"},
		{"org.mobyproject.extension.example.greeter.v0", "greeter"},
		{"moby.extensions.internal.launcher.echo.v1", "echo"},
	} {
		t.Run(string(tc.id), func(t *testing.T) {
			assert.Equal(t, tc.id.Name(), tc.want)
		})
	}
}

// TestEnabled reports on provider presence without resolving types.
func TestEnabled(t *testing.T) {
	assert.Assert(t, !testPoint.Enabled(resolverOf()))
	assert.Assert(t, testPoint.Enabled(resolverOf(ResolvedProvider{Extension: "a", Impl: callerFunc(nil)})))
}

// answer is the result type the Decide tests fan over; from records who
// decided.
type answer struct{ from string }

// TestDecide locks down the single-provider combinator: origin precedence, the
// one-provider limit, and the degrade-to-builtin convention -- an answer
// alongside an error -- that lets a point's caller apply a decision while
// surfacing the failure.
func TestDecide(t *testing.T) {
	call := func(from string, err error) ResolvedProvider {
		return ResolvedProvider{Extension: ExtensionID(from), Impl: callerFunc(func(context.Context) error { return err })}
	}
	builtin := func(from string, err error) ResolvedProvider {
		p := call(from, err)
		p.Builtin = true
		return p
	}
	// fn calls the provider and answers with the provider's extension id, so
	// assertions can tell who decided.
	decide := func(r Resolver) (answer, error) {
		return Decide(context.Background(), testPoint, r, Policy{}, func(ctx context.Context, c caller) (answer, error) {
			if err := c.Call(ctx); err != nil {
				return answer{}, err
			}
			return answer{from: "answered"}, nil
		})
	}

	t.Run("the effective provider answers", func(t *testing.T) {
		got, err := decide(resolverOf(builtin("org.stock.v1", nil), call("org.custom.v1", nil)))
		assert.NilError(t, err)
		assert.Equal(t, got.from, "answered")
	})

	t.Run("a failing provider degrades to the masked builtin", func(t *testing.T) {
		got, err := decide(resolverOf(builtin("org.stock.v1", nil), call("org.custom.v1", errors.New("down"))))
		assert.ErrorContains(t, err, `test provider "org.custom.v1": down`)
		assert.Equal(t, got.from, "answered", "the builtin must answer when the installed provider fails")
	})

	t.Run("a failing builtin has no fallback", func(t *testing.T) {
		got, err := decide(resolverOf(builtin("org.stock.v1", errors.New("stock down"))))
		assert.ErrorContains(t, err, "stock down")
		assert.Equal(t, got, answer{}, "there is no second opinion behind the builtin")
	})

	t.Run("both failing joins the errors", func(t *testing.T) {
		got, err := decide(resolverOf(builtin("org.stock.v1", errors.New("stock down")), call("org.custom.v1", errors.New("down"))))
		assert.ErrorContains(t, err, "down")
		assert.ErrorContains(t, err, "stock down")
		assert.Equal(t, got, answer{})
	})

	t.Run("no providers is an error", func(t *testing.T) {
		_, err := decide(resolverOf())
		assert.ErrorContains(t, err, "no providers")
	})

	t.Run("two installed providers are rejected", func(t *testing.T) {
		_, err := decide(resolverOf(call("org.one.v1", nil), call("org.two.v1", nil)))
		assert.ErrorContains(t, err, "multiple providers")
	})

	t.Run("the deadline reaches the provider", func(t *testing.T) {
		r := resolverOf(ResolvedProvider{Extension: "org.custom.v1", Impl: callerFunc(func(ctx context.Context) error {
			if _, ok := ctx.Deadline(); !ok {
				return errors.New("no deadline")
			}
			return nil
		})})
		_, err := Decide(context.Background(), testPoint, r, Policy{Timeout: time.Second}, func(ctx context.Context, c caller) (struct{}, error) {
			return struct{}{}, c.Call(ctx)
		})
		assert.NilError(t, err)
	})
}
