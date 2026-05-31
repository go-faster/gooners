package sshutil

import "context"

// RunWithContext runs a function f in a separate goroutine and returns its result
// or returns a context error if the context is canceled before f completes.
func RunWithContext[T any](ctx context.Context, f func() (T, error)) (T, error) {
	type result struct {
		val T
		err error
	}
	ch := make(chan result, 1)
	go func() {
		val, err := f()
		ch <- result{val, err}
	}()

	select {
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	case res := <-ch:
		return res.val, res.err
	}
}
