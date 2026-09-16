package provider

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/yousysadmin/lmnt/internal/guest"
)

// fakeProvider is a Provider whose guest is a channel.
type fakeProvider struct {
	startErr error

	mu      sync.Mutex
	exited  chan struct{}
	err     error
	stopped bool
}

func newFake() *fakeProvider { return &fakeProvider{exited: make(chan struct{})} }

func (f *fakeProvider) Start(context.Context) (guest.Executor, error) {
	if f.startErr != nil {
		f.die(nil)
		return nil, f.startErr
	}

	return nil, nil
}

func (f *fakeProvider) Exited() <-chan struct{} { return f.exited }

func (f *fakeProvider) Err() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.err
}

func (f *fakeProvider) Stop(context.Context) error {
	f.mu.Lock()
	f.stopped = true
	f.mu.Unlock()

	f.die(nil)

	return nil
}

func (f *fakeProvider) DiskDevice() string { return "vdb" }

// die ends the guest with err, once.
func (f *fakeProvider) die(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	select {
	case <-f.exited:
	default:
		f.err = err
		close(f.exited)
	}
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestRunContextCleanTask(t *testing.T) {
	p := newFake()

	err := RunContext(t.Context(), quietLogger(), p, func(context.Context, guest.Executor) error { return nil })
	if err != nil {
		t.Fatalf("got %v", err)
	}

	if !p.stopped {
		t.Error("provider was not stopped")
	}
}

func TestRunContextTaskError(t *testing.T) {
	p := newFake()
	boom := errors.New("boom")

	err := RunContext(t.Context(), quietLogger(), p, func(context.Context, guest.Executor) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("got %v, want boom", err)
	}
}

func TestRunContextInterruptedByCaller(t *testing.T) {
	p := newFake()
	ctx, cancel := context.WithCancelCause(t.Context())

	err := RunContext(ctx, quietLogger(), p, func(ctx context.Context, _ guest.Executor) error {
		cancel(ErrInterrupted)
		<-ctx.Done()

		return ctx.Err()
	})
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("got %v, want ErrInterrupted", err)
	}

	if !p.stopped {
		t.Error("provider was not stopped")
	}
}

func TestRunContextGuestDies(t *testing.T) {
	p := newFake()
	crash := errors.New("qemu crashed")

	err := RunContext(t.Context(), quietLogger(), p, func(ctx context.Context, _ guest.Executor) error {
		p.die(crash)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
			return errors.New("task was not cancelled when the guest died")
		}
	})
	if !errors.Is(err, crash) {
		t.Fatalf("got %v, want the crash", err)
	}
}

func TestRunContextStartFails(t *testing.T) {
	p := newFake()
	p.startErr = errors.New("no qemu")

	err := RunContext(t.Context(), quietLogger(), p, func(context.Context, guest.Executor) error {
		t.Error("task must not run")
		return nil
	})
	if err == nil || !errors.Is(err, p.startErr) {
		t.Fatalf("got %v", err)
	}
}

func TestRunContextStartInterrupted(t *testing.T) {
	p := newFake()
	p.startErr = context.Canceled

	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(ErrInterrupted)

	err := RunContext(ctx, quietLogger(), p, func(context.Context, guest.Executor) error { return nil })
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("got %v, want ErrInterrupted", err)
	}
}
