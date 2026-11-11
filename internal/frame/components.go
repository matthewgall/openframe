package frame

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
)

// Component is a named unit of frame background work (heartbeat, sync
// agent, update coordinator, sleep scheduler...).
//
// The runner enforces a hard bound on the number of concurrently
// executing components so a misbehaving component cannot create
// unbounded goroutines (spec §7: "Use bounded queues and avoid
// unbounded goroutine creation").
type Component struct {
	Name string
	// Run executes until ctx is cancelled; it must return promptly.
	Run func(ctx context.Context) error
}

// runner supervises a fixed set of components.
type runner struct {
	log *slog.Logger
	// maxComponents bounds concurrent component execution.
	maxComponents int
}

func newRunner(log *slog.Logger) *runner {
	return &runner{log: log, maxComponents: 8}
}

// Run starts all components, waits for ctx cancellation (or the first
// component failure) and then stops everything. It returns the first
// component error, if any, so the frame process can exit non-zero for
// watchdog/systemd to act on.
func (r *runner) Run(ctx context.Context, components []Component) error {
	if len(components) == 0 {
		<-ctx.Done()
		return ctx.Err()
	}
	if len(components) > r.maxComponents {
		return fmt.Errorf("too many components: %d > %d", len(components), r.maxComponents)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg       sync.WaitGroup
		sem      = make(chan struct{}, r.maxComponents)
		once     sync.Once
		firstErr atomic.Pointer[error]
	)

	setErr := func(err error) {
		if err == nil {
			return
		}
		once.Do(func() {
			// Do not treat a clean shutdown as a component error.
			if ctx.Err() != nil {
				return
			}
			firstErr.Store(&err)
			cancel()
		})
	}

	for _, c := range components {
		c := c
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			r.log.Debug("component started", "component", c.Name)
			err := c.Run(ctx)
			r.log.Debug("component stopped", "component", c.Name, "error", errString(err))
			if err != nil && ctx.Err() == nil {
				r.log.Error("component failed", "component", c.Name, "error", err)
				setErr(err)
			}
		}()
	}

	<-ctx.Done()
	wg.Wait()

	if e := firstErr.Load(); e != nil {
		return *e
	}
	return nil
}

func errString(err error) any {
	if err == nil {
		return nil
	}
	return err.Error()
}
