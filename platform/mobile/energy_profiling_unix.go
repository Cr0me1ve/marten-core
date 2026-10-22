//go:build android || linux || darwin

package mobile

import (
	"os"
	"os/signal"
	"syscall"
)

// c-shared Go needs SIGPROF routed through its runtime. The diagnostic APK
// enables this only in :core, never in the process hosting the Dart profiler.
func prepareEnergyProfileSignals() func() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGPROF)
	return func() { signal.Stop(ch) }
}
