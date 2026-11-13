package preflight

import (
	"context"
	"io"
	"testing"
	"time"
)

func TestReadInputCancellationClosesOpenPipe(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := ReadInput(ctx, reader, MaxInputBytes)
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if SafeCode(err) != "PREFLIGHT_CANCELLED" {
			t.Fatalf("cancelled input error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("open stdin pipe prevented cancellation")
	}
	// A pre-cancelled context may return before taking ownership of the reader.
	_ = reader.Close()
}

func TestReadInputDeadlineStopsUnfinishedInput(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	defer reader.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := ReadInput(ctx, reader, MaxInputBytes)
	if SafeCode(err) != "PREFLIGHT_TIMEOUT" {
		t.Fatalf("unfinished input ignored deadline: %v", err)
	}
}
