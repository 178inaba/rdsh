//go:build unix

package cmd

import (
	"os"
	"syscall"
	"time"
)

// deliveryGrace bounds the wait for the re-raised signal to arrive.
// Delivery takes microseconds when it happens at all, so the whole of this
// is only ever spent on a signal that cannot be delivered.
const deliveryGrace = 500 * time.Millisecond

// dieOfSignal ends the process by sig itself, so a parent sees WIFSIGNALED
// and reports 128 + signum. Execute's watcher has already put the default
// disposition back, so the signal terminates the process inside the wait
// below rather than returning from here.
//
// That wait is not redundant. syscall.Kill is not raise(3): it is not
// synchronous for the calling thread, and the runtime may deliver on
// another one, so execution can continue past the call for a few
// microseconds — long enough to reach a normal exit with a status that only
// spells the signal.
//
// It is bounded rather than indefinite because a signal can be undeliverable
// while Kill still reports success: inherited as SIG_IGN, which signal.Reset
// restores, or blocked in the inherited signal mask. A shell running rdsh as
// a background job does exactly that with SIGINT, and waiting forever there
// would wedge the process for good — a worse outcome than the misreported
// exit this all exists to fix. Go cannot force SIG_DFL the way C can, so
// falling back to the status a shell would have reported is all that is left.
func dieOfSignal(sig os.Signal) int {
	s, ok := sig.(syscall.Signal)
	if !ok {
		// Unreachable: every signal interruptSignals lists is one.
		return 1
	}
	if err := syscall.Kill(syscall.Getpid(), s); err != nil {
		return signalExitCode(s)
	}
	time.Sleep(deliveryGrace)
	return signalExitCode(s)
}

// signalExitCode is the status a shell reports for a process killed by sig.
// Only a fallback: dying of the signal is what lets a parent tell an
// interrupted run from one that merely exited with that number.
func signalExitCode(sig syscall.Signal) int {
	return 128 + int(sig)
}
