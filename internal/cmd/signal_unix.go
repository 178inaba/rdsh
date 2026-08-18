//go:build unix

package cmd

import (
	"os"
	"syscall"
)

// dieOfSignal ends the process by sig itself, so a parent sees WIFSIGNALED
// and reports 128 + signum. Execute's watcher has already put the default
// disposition back, so the signal terminates the process here.
//
// The wait is not redundant. syscall.Kill is not raise(3): it is not
// synchronous for the calling thread, and the runtime may deliver on
// another one, so execution can continue past the call for a few
// microseconds — long enough to reach a normal exit with a status that only
// spells the signal. Blocking keeps the signal from losing that race.
//
// A signal the process cannot receive — inherited as SIG_IGN, which
// signal.Reset restores, or blocked in the inherited mask — makes that wait
// permanent, because Kill still reports success. Go has no equivalent of
// forcing SIG_DFL first, so there is nothing to fall back to there.
func dieOfSignal(sig os.Signal) int {
	s, ok := sig.(syscall.Signal)
	if !ok {
		// Unreachable: every signal interruptSignals lists is one.
		return 1
	}
	if err := syscall.Kill(syscall.Getpid(), s); err != nil {
		return signalExitCode(s)
	}
	select {}
}

// signalExitCode is the status a shell reports for a process killed by sig.
// Only a fallback: dying of the signal is what lets a parent tell an
// interrupted run from one that merely exited with that number.
func signalExitCode(sig syscall.Signal) int {
	return 128 + int(sig)
}
