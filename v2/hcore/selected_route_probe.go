package hcore

import (
	"context"
	"time"

	"github.com/sagernet/sing-box/outbound"
)

// Both requests use the selected outbound. No cached result is used.
func probeIOSSelectedRoute(ctx context.Context, lifetime context.Context, probe func(context.Context, string) (uint16, error)) (uint16, error) {
	// The caller supplies the route-specific deadline. TURNcoat needs its full
	// startup window: an early cancellation closes the QUIC lease and can abort
	// the still-warming DTLS carrier. Ordinary and watchdog calls stay shorter.
	return probeSelectedRouteTargets(ctx, lifetime, 36*time.Second, probe)
}

func probeAndroidSelectedRoute(ctx context.Context, lifetime context.Context, probe func(context.Context, string) (uint16, error)) (uint16, error) {
	// Preserve the Box's platform/DNS context while propagating the admitted
	// Start/RPC cancellation. Stop must not wait for a probe on the Box lifetime
	// alone: that lifetime is closed only after the exact Start has finished.
	probeCtx, cancel := selectedRouteOperationContext(ctx, lifetime)
	defer cancel()
	delay, err := probeSelectedRouteTargets(probeCtx, lifetime, 8*time.Second, probe)
	// AfterFunc dispatch is asynchronous. A caller cancellation already visible
	// here wins even if its callback has not yet cancelled the merged context.
	if callerErr := ctx.Err(); callerErr != nil {
		return 0, callerErr
	}
	return delay, err
}

func selectedRouteOperationContext(ctx context.Context, lifetime context.Context) (context.Context, context.CancelFunc) {
	var operationCtx context.Context
	var cancel context.CancelFunc
	if deadline, ok := ctx.Deadline(); ok {
		operationCtx, cancel = context.WithDeadline(lifetime, deadline)
	} else {
		operationCtx, cancel = context.WithCancel(lifetime)
	}
	stopCallerCancellation := context.AfterFunc(ctx, cancel)
	if ctx.Err() != nil {
		cancel()
	}
	return operationCtx, func() {
		stopCallerCancellation()
		cancel()
	}
}

// Race independent control targets only during an explicit route operation.
// Cancellation closes losing requests; there is no idle monitor or retry loop.
func probeSelectedRouteTargets(ctx context.Context, lifetime context.Context, timeout time.Duration, probe func(context.Context, string) (uint16, error)) (uint16, error) {
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stopLifetimeCancellation := context.AfterFunc(lifetime, cancel)
	defer stopLifetimeCancellation()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := lifetime.Err(); err != nil {
		return 0, err
	}

	type result struct {
		delay uint16
		err   error
	}
	urls := [...]string{
		"https://www.gstatic.com/generate_204",
		"https://cp.cloudflare.com/generate_204",
	}
	results := make(chan result, len(urls))
	for _, url := range urls {
		go func() {
			delay, err := probe(probeCtx, url)
			results <- result{delay, err}
		}()
	}
	for range urls {
		select {
		case <-probeCtx.Done():
			return 0, probeCtx.Err()
		case response := <-results:
			// Cancellation wins over a concurrent successful response.
			if err := probeCtx.Err(); err != nil {
				return 0, err
			}
			if err := lifetime.Err(); err != nil {
				return 0, err
			}
			if response.err == nil && response.delay > 0 && response.delay < outbound.TimeoutDelay {
				return response.delay, nil
			}
		}
	}
	return outbound.TimeoutDelay, nil
}
