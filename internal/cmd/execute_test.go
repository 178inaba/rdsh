package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// helperEnv makes this binary run as rdsh itself rather than as a test
// suite. Execute ends a signalled run by re-raising the signal, which kills
// the process it runs in, so nothing inside the test process can observe
// the outcome — only a parent waiting on a child can.
const helperEnv = "RDSH_TEST_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "1" {
		os.Exit(Execute())
	}
	os.Exit(m.Run())
}

// rdshProcess is one run of rdsh as its own process: this test binary
// re-executed through the helper branch above.
type rdshProcess struct {
	cmd    *exec.Cmd
	stdout bytes.Buffer
	stderr bytes.Buffer
	waited chan error
}

// startRdsh runs rdsh against srv with a config directory of its own, and
// returns as soon as it has started.
func startRdsh(t *testing.T, srv *httptest.Server, args ...string) *rdshProcess {
	t.Helper()
	return start(t, srv, exec.Command(os.Args[0], args...))
}

// startRdshIgnoringInterrupt is startRdsh with SIGINT already set to
// SIG_IGN, which is what a shell hands a background job. An empty trap for
// INT sets that disposition and exec keeps it, so the process this returns
// is rdsh itself, ignoring the signal before it ever runs.
func startRdshIgnoringInterrupt(t *testing.T, srv *httptest.Server, args ...string) *rdshProcess {
	t.Helper()
	argv := append([]string{"-c", `trap '' INT; exec "$0" "$@"`, os.Args[0]}, args...)
	return start(t, srv, exec.Command("sh", argv...))
}

func start(t *testing.T, srv *httptest.Server, cmd *exec.Cmd) *rdshProcess {
	t.Helper()
	p := &rdshProcess{cmd: cmd}
	// Appended last so these win over anything the developer's own shell
	// exported: duplicate keys resolve to the last value.
	p.cmd.Env = append(os.Environ(),
		helperEnv+"=1",
		"XDG_CONFIG_HOME="+t.TempDir(),
		"RDSH_URL="+srv.URL,
		"RDSH_API_KEY=test-key",
	)
	p.cmd.Stdout = &p.stdout
	p.cmd.Stderr = &p.stderr
	if err := p.cmd.Start(); err != nil {
		t.Fatalf("starting rdsh: %v", err)
	}
	t.Cleanup(func() { _ = p.cmd.Process.Kill() })

	p.waited = make(chan error, 1)
	go func() { p.waited <- p.cmd.Wait() }()
	return p
}

func (p *rdshProcess) signal(t *testing.T, sig syscall.Signal) {
	t.Helper()
	if err := p.cmd.Process.Signal(sig); err != nil {
		t.Fatalf("sending %v to rdsh: %v", sig, err)
	}
}

// resendUntilExit sends sig every 20ms until rdsh exits. rdsh puts the
// default handler back a moment after the first signal, and that moment is
// not observable from here; resending is what keeps a test that needs the
// second signal to meet the default handler independent of it. Send errors
// are ignored — the process exiting is what ends the loop, and that is the
// same condition that makes a send fail.
func (p *rdshProcess) resendUntilExit(t *testing.T, sig syscall.Signal) {
	t.Helper()
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				_ = p.cmd.Process.Signal(sig)
			}
		}
	}()
}

// wait returns how the process ended, failing the test if it does not end.
func (p *rdshProcess) wait(t *testing.T) syscall.WaitStatus {
	t.Helper()
	timer := time.NewTimer(commandReturnTimeout)
	defer timer.Stop()
	select {
	case err := <-p.waited:
		var exitErr *exec.ExitError
		if err != nil && !errors.As(err, &exitErr) {
			t.Fatalf("waiting for rdsh: %v", err)
		}
		status, ok := p.cmd.ProcessState.Sys().(syscall.WaitStatus)
		if !ok {
			t.Fatalf("ProcessState.Sys() is %T, want syscall.WaitStatus", p.cmd.ProcessState.Sys())
		}
		return status
	case <-timer.C:
		t.Fatal("rdsh did not exit")
		// Unreachable: t.Fatal ends this goroutine. A zero value rather
		// than a literal because WaitStatus is a struct on Windows.
		var none syscall.WaitStatus
		return none
	}
}

// assertDiedOf checks what every run a signal ends must share: the process
// was killed by that signal itself — not merely exited with a status that
// spells it — and reported nothing on the way out.
func (p *rdshProcess) assertDiedOf(t *testing.T, sig syscall.Signal) {
	t.Helper()
	switch status := p.wait(t); {
	case !status.Signaled():
		t.Errorf("rdsh exited normally with status %d, want death by %v", status.ExitStatus(), sig)
	case status.Signal() != sig:
		t.Errorf("rdsh died of %v, want %v", status.Signal(), sig)
	}
	if out := p.stdout.String(); out != "" {
		t.Errorf("stdout = %q, want nothing", out)
	}
	if errOut := p.stderr.String(); errOut != "" {
		t.Errorf("stderr = %q, want no failure report", errOut)
	}
}

// assertExited checks a run that ends of its own accord, and returns its
// stderr for the caller to check the message on.
func (p *rdshProcess) assertExited(t *testing.T, code int) string {
	t.Helper()
	status := p.wait(t)
	if status.Signaled() {
		t.Fatalf("rdsh died of %v, want a normal exit with status %d", status.Signal(), code)
	}
	if got := status.ExitStatus(); got != code {
		t.Errorf("rdsh exit status = %d, want %d; stderr = %q", got, code, p.stderr.String())
	}
	return p.stderr.String()
}

// TestExecuteInterruptedRunDiesOfTheSignal covers the commands that wait on
// the server: a signal ends them with no failure report at all, and the
// process terminates by the signal so a parent sees WIFSIGNALED rather than
// an exit status that happens to spell one.
func TestExecuteInterruptedRunDiesOfTheSignal(t *testing.T) {
	tests := []struct {
		name    string
		server  *fakeServer
		args    []string
		waitFor string // the request that means the command has got there
		sig     syscall.Signal
	}{
		{
			name:    "query polling a job",
			server:  &fakeServer{neverDone: true},
			args:    []string{"run", "SELECT pg_sleep(600)", "--data-source", "5"},
			waitFor: pollRequest,
			sig:     syscall.SIGINT,
		},
		{
			name:    "query polling a job, terminated",
			server:  &fakeServer{neverDone: true},
			args:    []string{"run", "SELECT pg_sleep(600)", "--data-source", "5"},
			waitFor: pollRequest,
			sig:     syscall.SIGTERM,
		},
		{
			name:    "data-source list against a wedged server",
			server:  &fakeServer{hangList: true},
			args:    []string{"data-source", "list"},
			waitFor: dataSourcesRequest,
			sig:     syscall.SIGINT,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := tt.server.start(t)
			p := startRdsh(t, srv, tt.args...)
			tt.server.waitFor(t, tt.waitFor)
			p.signal(t, tt.sig)
			p.assertDiedOf(t, tt.sig)
		})
	}
}

// TestExecuteSecondSignalDuringJobCancellationEndsAtOnce drives the worst
// path in the issue this fixes: --timeout has already expired and its error
// is on its way up when the interrupt lands, with the job cancellation
// itself wedged. The run must not be reported as a timeout, and the second
// signal must not wait that cancellation out — either signal, not just a
// repeat of the first.
func TestExecuteSecondSignalDuringJobCancellationEndsAtOnce(t *testing.T) {
	// The wait being skipped is the client's 10s cancellation timeout;
	// well under it tells "ended at once" apart from "waited it out".
	const promptly = 3 * time.Second

	for _, second := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run("then "+second.String(), func(t *testing.T) {
			f := &fakeServer{neverDone: true, hangCancel: true}
			srv := f.start(t)
			p := startRdsh(t, srv, "run", "SELECT pg_sleep(600)", "--data-source", "5", "--timeout", "50ms")
			f.waitFor(t, cancelRequest)

			start := time.Now()
			p.signal(t, syscall.SIGINT)
			p.resendUntilExit(t, second)
			p.assertDiedOf(t, second)
			if elapsed := time.Since(start); elapsed > promptly {
				t.Errorf("rdsh took %s to end, want it not to wait the job cancellation out", elapsed)
			}
		})
	}
}

// TestExecuteUndeliverableSignalStillExits covers a signal rdsh cannot
// re-raise. A shell hands a background job SIGINT as SIG_IGN, signal.Reset
// puts that back, and Kill then reports success for a signal that never
// arrives. The run still has to end: waiting for a death that cannot come
// would wedge the process for good, which is worse than the misreported
// exit this all fixes. What is left is the status a shell would have
// reported had the signal landed.
func TestExecuteUndeliverableSignalStillExits(t *testing.T) {
	f := &fakeServer{neverDone: true}
	srv := f.start(t)

	p := startRdshIgnoringInterrupt(t, srv, "run", "SELECT pg_sleep(600)", "--data-source", "5")
	f.waitFor(t, pollRequest)
	p.signal(t, syscall.SIGINT)
	if errOut := p.assertExited(t, 128+int(syscall.SIGINT)); errOut != "" {
		t.Errorf("stderr = %q, want no failure report", errOut)
	}
}

// TestExecuteTimeoutExitsWithTimeoutCode pins the other half of the exit
// code contract: a --timeout expiry nobody interrupted is still 124 with
// its own message.
func TestExecuteTimeoutExitsWithTimeoutCode(t *testing.T) {
	f := &fakeServer{neverDone: true}
	srv := f.start(t)

	p := startRdsh(t, srv, "run", "SELECT pg_sleep(600)", "--data-source", "5", "--timeout", "50ms")
	if errOut := p.assertExited(t, timeoutExitCode); !strings.Contains(errOut, "query timed out") {
		t.Errorf("stderr = %q, want the timeout message", errOut)
	}
}

// TestExecuteOrdinaryFailureExitsOne pins the rest of it: a failure that is
// neither a timeout nor an interrupt still exits 1 and still says why.
func TestExecuteOrdinaryFailureExitsOne(t *testing.T) {
	srv := (&fakeServer{}).start(t)

	p := startRdsh(t, srv, "run", "SELECT 1", "--profile", "nope", "--data-source", "5")
	errOut := p.assertExited(t, 1)
	if !strings.Contains(errOut, "Error:") || !strings.Contains(errOut, "nope") {
		t.Errorf("stderr = %q, want a reported failure naming the profile", errOut)
	}
}

// TestExecuteCreatedButUnpublishedExitsOne covers the one place the exit
// code contract bends: a create that saved the query but could not publish
// it exits 1, not 124, and says where the draft is. The in-process helpers
// merge both streams into one buffer, so that stdout stays empty — no URL
// for a caller to read as a success — can only be checked out here.
func TestExecuteCreatedButUnpublishedExitsOne(t *testing.T) {
	f := &fakeServer{rejectPublish: true}
	srv := f.start(t)

	p := startRdsh(t, srv, "query", "create", "--name", "signups", "SELECT 1", "--data-source", "5")
	errOut := p.assertExited(t, 1)
	url := fmt.Sprintf("%s/queries/%d", srv.URL, savedQueryID)
	if !strings.Contains(errOut, url) || !strings.Contains(errOut, "draft") {
		t.Errorf("stderr = %q, want the query URL %s and that it remains a draft", errOut, url)
	}
	if out := p.stdout.String(); out != "" {
		t.Errorf("stdout = %q, want nothing a caller could read as the created URL", out)
	}
}
