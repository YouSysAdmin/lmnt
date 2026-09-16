// Package provider defines how lmnt obtains a running Linux guest and drives
// it through its life. A Provider boots the guest (a QEMU virtual machine or
// a privileged container), hands out a guest.Executor for it, and tears it
// down again. Run and RunContext are the shared lifecycle: start, do work,
// stop cleanly, and react to the guest dying or the user asking to stop.
package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yousysadmin/lmnt/internal/guest"
)

// Provider runs the Linux environment lmnt works in.
//
// Implementations must be safe to Stop from any goroutine, more than once,
// and even when Start failed or was never called.
type Provider interface {
	// Start boots the guest and returns once it accepts commands. When Start
	// returns an error the guest is already torn down and Exited is closed.
	// A cancelled ctx aborts the boot.
	Start(ctx context.Context) (guest.Executor, error)

	// Exited is closed once the guest is gone, for whatever reason.
	Exited() <-chan struct{}

	// Err reports why the guest exited: nil after a Stop, otherwise the
	// failure. It is meaningful only once Exited is closed.
	Err() error

	// Stop shuts the guest down, gracefully where possible, and returns when
	// it is gone or ctx is done. Gracefully means giving the guest a chance
	// to unmount and flush before the process is terminated.
	Stop(ctx context.Context) error

	// DiskDevice is the name under /dev at which the user's disk appears in
	// the guest ("vdb", "loop0"), or "" when no disk is attached. Valid
	// after Start returned nil.
	DiskDevice() string
}

// Task is the work run against a started guest. It should return when ctx is
// done, ctx is cancelled when the guest dies or the user asks to stop.
type Task func(ctx context.Context, ex guest.Executor) error

// ErrInterrupted is returned by Run and RunContext when the user stopped the
// session before the task finished. Callers of RunContext ask for that by
// cancelling ctx with this cause.
var ErrInterrupted = errors.New("interrupted")

// StopTimeout bounds how long a graceful shutdown may take.
const StopTimeout = 45 * time.Second

// Run is RunContext plus terminal signal handling: SIGINT, SIGTERM or SIGHUP
// (the terminal went away) ends the session as if ctx had been cancelled with
// ErrInterrupted. Further interrupts are ignored until the tenth, which
// terminates the process without cleanup. It is the lifecycle of the plain
// command-line commands.
func Run(ctx context.Context, logger *slog.Logger, p Provider, task Task) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(interrupts)

	go func() {
		for n := 1; ; n++ {
			select {
			case <-ctx.Done():
				return
			case sig := <-interrupts:
				switch {
				case n == 1:
					logger.Warn("Interrupted, shutting the guest down", "signal", sig.String())
					cancel(ErrInterrupted)
				case n < 10:
					logger.Warn("Shutdown in progress, interrupt again to force-quit", "remaining", 10-n)
				default:
					logger.Error("Force-quitting without cleanup")
					os.Exit(130)
				}
			}
		}
	}()

	return RunContext(ctx, logger, p, task)
}

// RunContext drives p through its whole life: boot it, run task against it,
// and stop it when task returns, the guest dies, or ctx is cancelled. It
// installs no signal handlers, a caller that wants to stop the session early
// cancels ctx, with ErrInterrupted as the cause when the stop is the user's
// wish rather than a failure.
//
// RunContext returns the task's error, ErrInterrupted when the session was
// stopped early on purpose, the guest's failure when it died on its own, or a
// stop error. A nil return means everything, including the shutdown, went
// well.
func RunContext(ctx context.Context, logger *slog.Logger, p Provider, task Task) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	ex, err := p.Start(ctx)
	if err != nil {
		if cause := context.Cause(ctx); errors.Is(cause, ErrInterrupted) {
			return ErrInterrupted
		}

		return fmt.Errorf("start guest: %w", err)
	}

	// The guest dying underneath the task must end the task.
	go func() {
		select {
		case <-p.Exited():
			cancel(fmt.Errorf("guest exited: %w", exitReason(p.Err())))
		case <-ctx.Done():
		}
	}()

	taskErr := task(ctx, ex)

	stopCtx, stopCancel := context.WithTimeout(context.WithoutCancel(ctx), StopTimeout)
	defer stopCancel()

	stopErr := p.Stop(stopCtx)
	<-p.Exited()

	cause := context.Cause(ctx)

	switch {
	case errors.Is(cause, ErrInterrupted):
		// The user ended the session, a task error caused by the
		// cancellation is expected noise.
		return errors.Join(ErrInterrupted, stopErr)
	case taskErr != nil && !errors.Is(taskErr, context.Canceled):
		return errors.Join(taskErr, stopErr)
	case p.Err() != nil:
		return errors.Join(fmt.Errorf("guest failed: %w", p.Err()), stopErr)
	case stopErr != nil:
		return fmt.Errorf("stop guest: %w", stopErr)
	case taskErr != nil:
		return taskErr
	default:
		return nil
	}
}

func exitReason(err error) error {
	if err == nil {
		return errors.New("clean exit")
	}

	return err
}
