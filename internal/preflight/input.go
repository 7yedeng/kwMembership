package preflight

import (
	"context"
	"io"
)

// ReadInput bounds stdin and lets the CLI stop on cancellation even when the
// producer keeps a pipe open. Closing an owned ReadCloser unblocks its read;
// the buffered result channel also lets a late generic Reader finish safely.
func ReadInput(ctx context.Context, reader io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 || limit > MaxInputBytes {
		return nil, fail("PREFLIGHT_INPUT_INVALID")
	}
	if err := ctx.Err(); err != nil {
		return nil, fail(SafeCode(err))
	}
	type readResult struct {
		data []byte
		err  error
	}
	result := make(chan readResult, 1)
	go func() {
		data, err := readBounded(reader, limit)
		result <- readResult{data: data, err: err}
	}()
	select {
	case <-ctx.Done():
		if closer, ok := reader.(io.Closer); ok {
			_ = closer.Close()
		}
		return nil, fail(SafeCode(ctx.Err()))
	case value := <-result:
		if err := ctx.Err(); err != nil {
			return nil, fail(SafeCode(err))
		}
		return value.data, value.err
	}
}
