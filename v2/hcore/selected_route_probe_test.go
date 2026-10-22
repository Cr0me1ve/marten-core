package hcore

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagernet/sing-box/outbound"
)

const selectedRouteProbeTestWait = time.Second

func waitForSelectedRouteProbeSignal(t *testing.T, signal <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(selectedRouteProbeTestWait):
		t.Fatal(message)
	}
}

func waitForSelectedRouteProbeError(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(selectedRouteProbeTestWait):
		t.Fatal("selected-route probe did not return")
		return nil
	}
}

func TestProbeIOSSelectedRouteReturnsSecondSuccessAfterFirstFailure(t *testing.T) {
	var calls atomic.Int32
	delay, err := probeIOSSelectedRoute(context.Background(), context.Background(), func(context.Context, string) (uint16, error) {
		if calls.Add(1) == 1 {
			return 0, errors.New("first probe failed")
		}
		return 41, nil
	})

	if err != nil {
		t.Fatalf("probeIOSSelectedRoute error = %v, want nil", err)
	}
	if delay != 41 {
		t.Fatalf("probeIOSSelectedRoute delay = %d, want 41", delay)
	}
}

func TestProbeIOSSelectedRouteGivesBothRequestsThirtySixSecondBudget(t *testing.T) {
	deadlines := make(chan time.Duration, 2)
	release := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		_, err := probeIOSSelectedRoute(context.Background(), context.Background(), func(ctx context.Context, _ string) (uint16, error) {
			deadline, ok := ctx.Deadline()
			if !ok {
				return 0, errors.New("probe context has no deadline")
			}
			deadlines <- time.Until(deadline)
			<-release
			return 31, nil
		})
		result <- err
	}()

	for range 2 {
		budget := waitForSelectedRouteProbeBudget(t, deadlines)
		if budget < 35*time.Second || budget > 37*time.Second {
			t.Fatalf("probe context budget = %v, want approximately 36s", budget)
		}
	}
	close(release)
	if err := waitForSelectedRouteProbeError(t, result); err != nil {
		t.Fatalf("probeIOSSelectedRoute error = %v, want nil", err)
	}
}

func TestProbeIOSSelectedRouteHonorsShorterCallerDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	t.Cleanup(cancel)
	deadlines := make(chan time.Duration, 2)
	release := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		_, err := probeIOSSelectedRoute(ctx, context.Background(), func(probeCtx context.Context, _ string) (uint16, error) {
			deadline, ok := probeCtx.Deadline()
			if !ok {
				return 0, errors.New("probe context has no deadline")
			}
			deadlines <- time.Until(deadline)
			<-release
			return 31, nil
		})
		result <- err
	}()

	for range 2 {
		budget := waitForSelectedRouteProbeBudget(t, deadlines)
		if budget < 7*time.Second || budget > 9*time.Second {
			t.Fatalf("probe context budget = %v, want caller-clamped approximately 8s", budget)
		}
	}
	close(release)
	if err := waitForSelectedRouteProbeError(t, result); err != nil {
		t.Fatalf("probeIOSSelectedRoute error = %v, want nil", err)
	}
}

func waitForSelectedRouteProbeBudget(t *testing.T, deadlines <-chan time.Duration) time.Duration {
	t.Helper()
	select {
	case budget := <-deadlines:
		return budget
	case <-time.After(selectedRouteProbeTestWait):
		t.Fatal("probe callback did not receive a deadline")
		return 0
	}
}

func TestProbeIOSSelectedRouteReturnsSuccessAndCancelsStalledLoser(t *testing.T) {
	started := make(chan struct{}, 2)
	allowSuccess := make(chan struct{})
	loserCancelled := make(chan struct{}, 1)
	var calls atomic.Int32
	result := make(chan struct {
		delay uint16
		err   error
	}, 1)

	go func() {
		delay, err := probeIOSSelectedRoute(context.Background(), context.Background(), func(ctx context.Context, _ string) (uint16, error) {
			call := calls.Add(1)
			started <- struct{}{}
			if call == 1 {
				<-ctx.Done()
				loserCancelled <- struct{}{}
				return 0, ctx.Err()
			}
			<-allowSuccess
			return 52, nil
		})
		result <- struct {
			delay uint16
			err   error
		}{delay, err}
	}()

	waitForSelectedRouteProbeSignal(t, started, "first probe did not start")
	waitForSelectedRouteProbeSignal(t, started, "second probe did not start")
	close(allowSuccess)

	select {
	case got := <-result:
		if got.err != nil {
			t.Fatalf("probeIOSSelectedRoute error = %v, want nil", got.err)
		}
		if got.delay != 52 {
			t.Fatalf("probeIOSSelectedRoute delay = %d, want 52", got.delay)
		}
	case <-time.After(selectedRouteProbeTestWait):
		t.Fatal("successful probe did not return")
	}
	waitForSelectedRouteProbeSignal(t, loserCancelled, "stalled loser was not cancelled after success")
}

func TestProbeIOSSelectedRouteReturnsTimeoutWhenBothProbesFail(t *testing.T) {
	delay, err := probeIOSSelectedRoute(context.Background(), context.Background(), func(context.Context, string) (uint16, error) {
		return 0, errors.New("probe failed")
	})

	if err != nil {
		t.Fatalf("probeIOSSelectedRoute error = %v, want nil", err)
	}
	if delay != outbound.TimeoutDelay {
		t.Fatalf("probeIOSSelectedRoute delay = %d, want timeout %d", delay, outbound.TimeoutDelay)
	}
}

func TestProbeIOSSelectedRouteRejectsInvalidSuccessfulDelays(t *testing.T) {
	for _, delay := range []uint16{0, outbound.TimeoutDelay} {
		t.Run("delay", func(t *testing.T) {
			got, err := probeIOSSelectedRoute(context.Background(), context.Background(), func(context.Context, string) (uint16, error) {
				return delay, nil
			})

			if err != nil {
				t.Fatalf("probeIOSSelectedRoute error = %v, want nil", err)
			}
			if got != outbound.TimeoutDelay {
				t.Fatalf("probeIOSSelectedRoute delay = %d, want timeout %d", got, outbound.TimeoutDelay)
			}
		})
	}
}

func TestProbeIOSSelectedRouteCancelsBothProbesWhenCallerCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	assertProbeIOSSelectedRouteCancellation(t, ctx, context.Background(), cancel)
}

func TestProbeIOSSelectedRouteCancelsBothProbesWhenLifetimeCancels(t *testing.T) {
	lifetime, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	assertProbeIOSSelectedRouteCancellation(t, context.Background(), lifetime, cancel)
}

func assertProbeIOSSelectedRouteCancellation(
	t *testing.T,
	ctx context.Context,
	lifetime context.Context,
	cancel context.CancelFunc,
) {
	t.Helper()
	started := make(chan struct{}, 2)
	cancelled := make(chan struct{}, 2)
	result := make(chan error, 1)

	go func() {
		_, err := probeIOSSelectedRoute(ctx, lifetime, func(probeCtx context.Context, _ string) (uint16, error) {
			started <- struct{}{}
			<-probeCtx.Done()
			cancelled <- struct{}{}
			return 0, probeCtx.Err()
		})
		result <- err
	}()

	waitForSelectedRouteProbeSignal(t, started, "first probe did not start")
	waitForSelectedRouteProbeSignal(t, started, "second probe did not start")
	cancel()

	if err := waitForSelectedRouteProbeError(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("probeIOSSelectedRoute error = %v, want context cancellation", err)
	}
	waitForSelectedRouteProbeSignal(t, cancelled, "first probe was not cancelled")
	waitForSelectedRouteProbeSignal(t, cancelled, "second probe was not cancelled")
}

func TestProbeAndroidSelectedRouteReturnsSecondSuccessAfterFirstFailure(t *testing.T) {
	var calls atomic.Int32
	delay, err := probeAndroidSelectedRoute(context.Background(), context.Background(), func(context.Context, string) (uint16, error) {
		if calls.Add(1) == 1 {
			return 0, errors.New("first probe failed")
		}
		return 43, nil
	})

	if err != nil {
		t.Fatalf("probeAndroidSelectedRoute error = %v, want nil", err)
	}
	if delay != 43 {
		t.Fatalf("probeAndroidSelectedRoute delay = %d, want 43", delay)
	}
}

func TestProbeAndroidSelectedRouteReturnsSuccessAndCancelsStalledLoser(t *testing.T) {
	started := make(chan struct{}, 2)
	allowSuccess := make(chan struct{})
	loserCancelled := make(chan struct{}, 1)
	var calls atomic.Int32
	result := make(chan struct {
		delay uint16
		err   error
	}, 1)

	go func() {
		delay, err := probeAndroidSelectedRoute(context.Background(), context.Background(), func(ctx context.Context, _ string) (uint16, error) {
			call := calls.Add(1)
			started <- struct{}{}
			if call == 1 {
				<-ctx.Done()
				loserCancelled <- struct{}{}
				return 0, ctx.Err()
			}
			<-allowSuccess
			return 54, nil
		})
		result <- struct {
			delay uint16
			err   error
		}{delay, err}
	}()

	waitForSelectedRouteProbeSignal(t, started, "first Android probe did not start")
	waitForSelectedRouteProbeSignal(t, started, "second Android probe did not start")
	close(allowSuccess)

	select {
	case got := <-result:
		if got.err != nil {
			t.Fatalf("probeAndroidSelectedRoute error = %v, want nil", got.err)
		}
		if got.delay != 54 {
			t.Fatalf("probeAndroidSelectedRoute delay = %d, want 54", got.delay)
		}
	case <-time.After(selectedRouteProbeTestWait):
		t.Fatal("successful Android probe did not return")
	}
	waitForSelectedRouteProbeSignal(t, loserCancelled, "stalled Android loser was not cancelled after success")
}

func TestProbeAndroidSelectedRouteCancelsBothProbesWhenCallerCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	assertProbeAndroidSelectedRouteCancellation(t, ctx, context.Background(), cancel)
}

func TestProbeAndroidSelectedRouteCancelsBothProbesWhenLifetimeCancels(t *testing.T) {
	lifetime, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	assertProbeAndroidSelectedRouteCancellation(t, context.Background(), lifetime, cancel)
}

func assertProbeAndroidSelectedRouteCancellation(
	t *testing.T,
	ctx context.Context,
	lifetime context.Context,
	cancel context.CancelFunc,
) {
	t.Helper()
	started := make(chan struct{}, 2)
	cancelled := make(chan struct{}, 2)
	result := make(chan error, 1)

	go func() {
		_, err := probeAndroidSelectedRoute(ctx, lifetime, func(probeCtx context.Context, _ string) (uint16, error) {
			started <- struct{}{}
			<-probeCtx.Done()
			cancelled <- struct{}{}
			return 0, probeCtx.Err()
		})
		result <- err
	}()

	waitForSelectedRouteProbeSignal(t, started, "first Android probe did not start")
	waitForSelectedRouteProbeSignal(t, started, "second Android probe did not start")
	cancel()

	if err := waitForSelectedRouteProbeError(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("probeAndroidSelectedRoute error = %v, want context cancellation", err)
	}
	waitForSelectedRouteProbeSignal(t, cancelled, "first Android probe was not cancelled")
	waitForSelectedRouteProbeSignal(t, cancelled, "second Android probe was not cancelled")
}

func TestProbeSelectedRouteTargetsHonorsShortTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	started := make(chan struct{}, 2)
	result := make(chan error, 1)
	go func() {
		_, err := probeSelectedRouteTargets(ctx, context.Background(), 40*time.Millisecond, func(probeCtx context.Context, _ string) (uint16, error) {
			started <- struct{}{}
			<-probeCtx.Done()
			return 0, probeCtx.Err()
		})
		result <- err
	}()

	waitForSelectedRouteProbeSignal(t, started, "first target probe did not start")
	waitForSelectedRouteProbeSignal(t, started, "second target probe did not start")
	if err := waitForSelectedRouteProbeError(t, result); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("probeSelectedRouteTargets error = %v, want bounded probe deadline", err)
	}
}

func TestProbeAndroidSelectedRoutePreservesLifetimeValuesAndCallerDeadline(t *testing.T) {
	type lifetimeValueKey struct{}
	lifetime := context.WithValue(context.Background(), lifetimeValueKey{}, "box-lifetime")
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	t.Cleanup(cancel)
	started := make(chan struct{}, 2)
	result := make(chan error, 1)
	go func() {
		_, err := probeAndroidSelectedRoute(ctx, lifetime, func(probeCtx context.Context, _ string) (uint16, error) {
			if got := probeCtx.Value(lifetimeValueKey{}); got != "box-lifetime" {
				return 0, errors.New("probe context lost lifetime value")
			}
			deadline, ok := probeCtx.Deadline()
			if !ok {
				return 0, errors.New("probe context has no deadline")
			}
			if remaining := time.Until(deadline); remaining > 60*time.Millisecond {
				return 0, errors.New("probe deadline did not preserve shorter caller deadline")
			}
			started <- struct{}{}
			<-probeCtx.Done()
			return 0, probeCtx.Err()
		})
		result <- err
	}()

	waitForSelectedRouteProbeSignal(t, started, "first Android target probe did not start")
	waitForSelectedRouteProbeSignal(t, started, "second Android target probe did not start")
	if err := waitForSelectedRouteProbeError(t, result); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("probeAndroidSelectedRoute error = %v, want caller deadline", err)
	}
}

func TestProbeSelectedRouteTargetsRejectsSuccessAfterCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	delay, err := probeSelectedRouteTargets(ctx, context.Background(), time.Second, func(context.Context, string) (uint16, error) {
		cancel()
		return 55, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("probeSelectedRouteTargets error = %v, want cancellation to defeat raced success", err)
	}
	if delay != 0 {
		t.Fatalf("probeSelectedRouteTargets delay = %d, want zero when cancellation wins", delay)
	}
}
