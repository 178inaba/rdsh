package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/178inaba/rdsh/internal/config"
)

// runRdshIn is runRdsh with an explicit config dir so tests can seed or
// inspect the config file. srv == nil leaves the env pair unset.
func runRdshIn(t *testing.T, configDir string, srv *httptest.Server, stdin string, args ...string) (string, error) {
	t.Helper()
	setRdshEnv(t, configDir, srv)
	return runRdshWithEnvSet(t, stdin, args...)
}

// setRdshEnv points rdsh at srv with a config directory of its own, or at
// no server at all when srv is nil. Every helper that runs a command in
// process goes through here, so what a test run inherits is described once.
func setRdshEnv(t *testing.T, configDir string, srv *httptest.Server) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", configDir)
	if srv != nil {
		t.Setenv("RDSH_URL", srv.URL)
		t.Setenv("RDSH_API_KEY", "test-key")
		return
	}
	t.Setenv("RDSH_URL", "")
	t.Setenv("RDSH_API_KEY", "")
	os.Unsetenv("RDSH_URL")
	os.Unsetenv("RDSH_API_KEY")
}

func TestAuthLoginSavesVerifiedProfile(t *testing.T) {
	f := &fakeServer{}
	srv := f.start(t)
	dir := t.TempDir()

	// Prompts answered in order: URL, API key, profile name, default data source.
	stdin := fmt.Sprintf("%s\nsecret-key\nstaging\n3\n", srv.URL)
	out, err := runRdshIn(t, dir, nil, stdin, "auth", "login")
	if err != nil {
		t.Fatalf("auth login error = %v (output: %s)", err, out)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	p, ok := cfg.Profiles["staging"]
	if !ok {
		t.Fatalf("profile staging not saved: %+v", cfg)
	}
	if p.URL != srv.URL || p.APIKey != "secret-key" || p.DataSource != "3" {
		t.Errorf("saved profile = %+v", p)
	}
	if cfg.ActiveProfile != "staging" {
		t.Errorf("first profile should become active, got %q", cfg.ActiveProfile)
	}
}

func TestAuthLoginDefaultsProfileNameAndDataSourceOptional(t *testing.T) {
	f := &fakeServer{}
	srv := f.start(t)
	dir := t.TempDir()

	stdin := fmt.Sprintf("%s\nsecret-key\n\n\n", srv.URL)
	if _, err := runRdshIn(t, dir, nil, stdin, "auth", "login"); err != nil {
		t.Fatalf("auth login error = %v", err)
	}

	cfg, _ := config.Load()
	p, ok := cfg.Profiles["default"]
	if !ok {
		t.Fatalf("profile default not saved: %+v", cfg)
	}
	if p.DataSource != "" {
		t.Errorf("DataSource = %q, want empty", p.DataSource)
	}
}

func TestAuthLoginRejectedCredentialsSaveNothing(t *testing.T) {
	f := &fakeServer{rejectSession: true}
	srv := f.start(t)
	dir := t.TempDir()

	stdin := fmt.Sprintf("%s\nbad-key\nstaging\n", srv.URL)
	_, err := runRdshIn(t, dir, nil, stdin, "auth", "login")
	if err == nil {
		t.Fatal("auth login with rejected credentials: want error")
	}

	path, _ := config.Path()
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("config file should not exist after failed login, stat err = %v", statErr)
	}
}

// authLoginPrompts is what auth login writes, in order, for a run that
// answers every prompt.
var authLoginPrompts = []string{
	"Redash URL: ",
	"API key: ",
	"Profile name [default]: ",
	"Default data source (ID or name, optional): ",
}

// authLoginAnswers answers authLoginPrompts one for one.
func authLoginAnswers(srvURL string) []string {
	return []string{srvURL + "\n", "secret-key\n", "staging\n", "3\n"}
}

// scriptedStdin serves one answer per Read so a test can pick the prompt
// the user interrupts. interrupt fires as the read at interruptAt is
// served; when that read has no answer left — the user pressed Ctrl-C
// instead of typing — the reader then blocks the way a real terminal read
// does. Go registers its signal handlers with SA_RESTART, so a signal does
// not make a pending read return; returning io.EOF here would end the
// prompt on its own and the command would abort with or without the fix
// under test.
type scriptedStdin struct {
	answers     []string // each ends with "\n"
	interruptAt int
	interrupt   func()
	release     <-chan struct{} // closed by the test, releasing a parked read

	reads int
}

func (s *scriptedStdin) Read(p []byte) (int, error) {
	read := s.reads
	s.reads++
	if read == s.interruptAt {
		s.interrupt()
	}
	if read < len(s.answers) {
		return copy(p, s.answers[read]), nil
	}
	<-s.release
	return 0, io.EOF
}

func newScriptedStdin(t *testing.T, interrupt func(), interruptAt int, answers []string) *scriptedStdin {
	t.Helper()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	return &scriptedStdin{
		answers:     answers,
		interruptAt: interruptAt,
		interrupt:   interrupt,
		release:     release,
	}
}

// newInterruptedStdin answers the prompts in order and then interrupts at
// the next one, standing in for a user who presses Ctrl-C instead of typing.
func newInterruptedStdin(t *testing.T, interrupt func(), answers ...string) *scriptedStdin {
	t.Helper()
	return newScriptedStdin(t, interrupt, len(answers), answers)
}

// assertInterrupted checks what an interrupted run looks like below
// Execute: the command unwinds with the interruption sentinel rather than
// the empty-answer complaint an unwatched prompt used to produce, and that
// sentinel is not one exitCode singles out. A real run never gets as far as
// that exit code — Execute dies of the signal first (execute_test.go) — but
// this is the mapping that would apply if it did.
func assertInterrupted(t *testing.T, out string, err error) {
	t.Helper()
	if !errors.Is(err, errInterrupted) {
		t.Fatalf("error = %v, want %v; output = %q", err, errInterrupted, out)
	}
	if got := exitCode(err); got != 1 {
		t.Errorf("exitCode(%v) = %d, want 1", err, got)
	}
}

// readConfigFile returns the config file's contents, or nil when it does
// not exist.
func readConfigFile(t *testing.T) []byte {
	t.Helper()
	path, err := config.Path()
	if err != nil {
		t.Fatalf("config.Path() error = %v", err)
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("reading the config file: %v", err)
	}
	return data
}

// assertConfigUnchanged checks the config file against what it held before
// the run — including never having existed, for which want is nil.
func assertConfigUnchanged(t *testing.T, want []byte) {
	t.Helper()
	got := readConfigFile(t)
	if want == nil {
		if got != nil {
			t.Errorf("config file was created, contents = %s", got)
		}
		return
	}
	if !bytes.Equal(got, want) {
		t.Errorf("config file = %s, want it unchanged at %s", got, want)
	}
}

// TestAuthLoginInterruptedAtPromptSavesNothing covers Ctrl-C at each prompt.
// The data source cases are the ones that used to save the profile and exit
// 0: the key has already been verified by then, so nothing downstream
// noticed that the user had given up, and the empty answer their later
// Enter produced was valid for an optional prompt.
func TestAuthLoginInterruptedAtPromptSavesNothing(t *testing.T) {
	tests := []struct {
		name string
		// interruptAt indexes authLoginPrompts: every earlier prompt is
		// answered and the run is interrupted at this one.
		interruptAt int
		seed        *config.Config
	}{
		{name: "the URL prompt", interruptAt: 0},
		{name: "the API key prompt", interruptAt: 1},
		{name: "the profile name prompt", interruptAt: 2},
		{name: "the data source prompt", interruptAt: 3},
		{
			name:        "the data source prompt over an existing profile",
			interruptAt: 3,
			seed: &config.Config{
				ActiveProfile: "staging",
				Profiles:      map[string]config.Profile{"staging": {URL: "https://old.example.com", APIKey: "old-key"}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			if tt.seed != nil {
				if err := tt.seed.Save(); err != nil {
					t.Fatalf("seeding the config: %v", err)
				}
			}
			before := readConfigFile(t)
			srv := (&fakeServer{}).start(t)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			stdin := newInterruptedStdin(t, cancel, authLoginAnswers(srv.URL)[:tt.interruptAt]...)
			out, err := runRdshWithStdin(t, ctx, stdin, "auth", "login")
			assertInterrupted(t, out, err)

			// Pinning the whole output covers three things at once: the run
			// stopped at this prompt, the cancelled prompt ended its own
			// line so the error starts on a fresh one, and nothing reached
			// stdout (the two streams share this buffer).
			want := strings.Join(authLoginPrompts[:tt.interruptAt+1], "") + "\n"
			if out != want {
				t.Errorf("output = %q, want %q", out, want)
			}
			assertConfigUnchanged(t, before)
		})
	}
}

// TestAuthLoginInterruptedWithFinalAnswerSavesNothing covers a signal that
// arrives together with the final answer rather than in place of it: the
// run must still save nothing.
//
// It does not reach runAuthLogin's pre-save context check. Cancelling from
// inside the read means the context is already done by the time the prompt
// selects on it, so the prompt wins every time (verified by deleting the
// check: this test still passed 500 runs). That check covers a signal
// landing after the last prompt returned, which nothing here can schedule —
// it is a second line of defence, not the mechanism under test.
func TestAuthLoginInterruptedWithFinalAnswerSavesNothing(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	srv := (&fakeServer{}).start(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Interrupt as the final answer is served, rather than in place of it.
	answers := authLoginAnswers(srv.URL)
	stdin := newScriptedStdin(t, cancel, len(answers)-1, answers)
	out, err := runRdshWithStdin(t, ctx, stdin, "auth", "login")
	assertInterrupted(t, out, err)
	assertConfigUnchanged(t, nil)
}

// TestAuthLoginSignalAtPromptAborts drives the real signals rather than a
// bare context cancellation, so SIGTERM is covered alongside SIGINT.
func TestAuthLoginSignalAtPromptAborts(t *testing.T) {
	// Pinning the list rather than just iterating it: iterating alone would
	// still pass if Execute stopped registering SIGTERM.
	signals := []os.Signal{os.Interrupt, syscall.SIGTERM}
	if got := interruptSignals(); !slices.Equal(got, signals) {
		t.Fatalf("interruptSignals() = %v, want %v", got, signals)
	}

	self, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatalf("os.FindProcess() error = %v", err)
	}

	for _, sig := range signals {
		t.Run(sig.String(), func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			srv := (&fakeServer{}).start(t)

			// Handling the signal here is what keeps it from terminating
			// the test binary; Execute is deliberately not in the way,
			// since it would answer by re-raising and killing this
			// process. What that leaves observable is the prompt aborting.
			ctx, stop := signal.NotifyContext(context.Background(), sig)
			defer stop()

			stdin := newInterruptedStdin(t, func() {
				if err := self.Signal(sig); err != nil {
					t.Errorf("sending %v to the test process: %v", sig, err)
				}
			}, authLoginAnswers(srv.URL)[:2]...)

			out, err := runRdshWithStdin(t, ctx, stdin, "auth", "login")
			assertInterrupted(t, out, err)
			assertConfigUnchanged(t, nil)
		})
	}
}

// TestAuthLoginRejectsNonTTYStdin pins the guard that sends piped callers to
// the env pair: the masked prompt cannot run without a terminal.
func TestAuthLoginRejectsNonTTYStdin(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	// A pipe is an *os.File that is not a terminal — what stdin looks like
	// under `rdsh auth login < /dev/null`.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error = %v", err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Errorf("closing the read end of the pipe: %v", err)
		}
	})
	if _, err := w.WriteString("https://redash.example.com\n"); err != nil {
		t.Fatalf("writing to the pipe: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("closing the write end of the pipe: %v", err)
	}

	out, err := runRdshWithStdin(t, context.Background(), r, "auth", "login")
	if err == nil || !strings.Contains(err.Error(), "needs a terminal") {
		t.Fatalf("error = %v, want the needs-a-terminal error", err)
	}
	if out != "" {
		t.Errorf("output = %q, want the command to refuse before prompting", out)
	}
	assertConfigUnchanged(t, nil)
}

func TestProfileUseAndList(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	seed := &config.Config{
		ActiveProfile: "dev",
		Profiles: map[string]config.Profile{
			"dev":  {URL: "https://dev.example.com", APIKey: "k1"},
			"prod": {URL: "https://prod.example.com", APIKey: "k2"},
		},
	}
	if err := seed.Save(); err != nil {
		t.Fatal(err)
	}

	out, err := runRdshIn(t, dir, nil, "", "profile", "list")
	if err != nil {
		t.Fatalf("profile list error = %v", err)
	}
	if !strings.Contains(out, "* dev") || !strings.Contains(out, "prod") {
		t.Errorf("profile list output = %q, want active marker on dev", out)
	}

	if _, err := runRdshIn(t, dir, nil, "", "profile", "use", "prod"); err != nil {
		t.Fatalf("profile use error = %v", err)
	}
	cfg, _ := config.Load()
	if cfg.ActiveProfile != "prod" {
		t.Errorf("ActiveProfile = %q, want prod", cfg.ActiveProfile)
	}
}

func TestProfileUseUnknown(t *testing.T) {
	dir := t.TempDir()
	_, err := runRdshIn(t, dir, nil, "", "profile", "use", "nope")
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("error = %v, want unknown-profile error naming it", err)
	}
}

func TestDataSourceList(t *testing.T) {
	f := &fakeServer{}
	srv := f.start(t)

	out, err := runRdshIn(t, t.TempDir(), srv, "", "data-source", "list")
	if err != nil {
		t.Fatalf("data-source list error = %v", err)
	}
	if want := "7\twarehouse\n"; out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
}

// TestDataSourceListTimeout covers the wedged server the listing used to
// block on forever: the deadline has to end the run, and say what it was
// waiting for.
func TestDataSourceListTimeout(t *testing.T) {
	f := &fakeServer{hangList: true}
	srv := f.start(t)

	_, err := runRdshIn(t, t.TempDir(), srv, "", "data-source", "list", "--timeout", "50ms")
	if !errors.Is(err, errTimeout) {
		t.Fatalf("error = %v, want errTimeout", err)
	}
	if got := exitCode(err); got != timeoutExitCode {
		t.Errorf("exitCode(%v) = %d, want %d", err, got, timeoutExitCode)
	}
	if !strings.Contains(err.Error(), "data source") {
		t.Errorf("error = %v, want it to name the operation that timed out", err)
	}
}

// TestDataSourceListNegativeTimeout checks that a bad duration is refused
// before the listing is requested, not after the server has answered it.
func TestDataSourceListNegativeTimeout(t *testing.T) {
	f := &fakeServer{}
	srv := f.start(t)

	_, err := runRdshIn(t, t.TempDir(), srv, "", "data-source", "list", "--timeout", "-5s")
	if err == nil || !strings.Contains(err.Error(), "negative") {
		t.Fatalf("error = %v, want negative-timeout error", err)
	}
	select {
	case got := <-f.reached:
		t.Errorf("the %s request was sent despite the invalid --timeout", got)
	default:
	}
}

// TestDataSourceListUnlimitedTimeout pins that 0 means no limit rather
// than a deadline that has already passed.
func TestDataSourceListUnlimitedTimeout(t *testing.T) {
	f := &fakeServer{}
	srv := f.start(t)

	out, err := runRdshIn(t, t.TempDir(), srv, "", "data-source", "list", "--timeout", "0")
	if err != nil {
		t.Fatalf("data-source list error = %v", err)
	}
	if want := "7\twarehouse\n"; out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
}

// TestAuthLoginVerificationTimeout covers a server that takes the
// connection and never answers the verification. Nothing is saved: the run
// ends at the deadline instead of hanging on a request no signal but the
// user's own could end.
func TestAuthLoginVerificationTimeout(t *testing.T) {
	f := &fakeServer{hangSession: true}
	srv := f.start(t)
	dir := t.TempDir()

	stdin := fmt.Sprintf("%s\nsecret-key\nstaging\n3\n", srv.URL)
	_, err := runRdshIn(t, dir, nil, stdin, "auth", "login", "--timeout", "50ms")
	if !errors.Is(err, errTimeout) {
		t.Fatalf("error = %v, want errTimeout", err)
	}
	if got := exitCode(err); got != timeoutExitCode {
		t.Errorf("exitCode(%v) = %d, want %d", err, got, timeoutExitCode)
	}
	assertConfigUnchanged(t, nil)
}

// slowStdin answers the prompts in order, pausing before the reads named in
// delayed so that their answers arrive well after --timeout would have
// expired. A prompt bounded by the verification's deadline would abort
// there rather than wait.
type slowStdin struct {
	answers []string // each ends with "\n"
	delayed map[int]time.Duration

	reads int
}

func (s *slowStdin) Read(p []byte) (int, error) {
	read := s.reads
	s.reads++
	if delay, ok := s.delayed[read]; ok {
		time.Sleep(delay)
	}
	if read < len(s.answers) {
		return copy(p, s.answers[read]), nil
	}
	return 0, io.EOF
}

// TestAuthLoginPromptsOutliveTheTimeout pins that --timeout bounds the
// verification alone. The prompt before it would fail if the whole run were
// bounded, and the one after it if the derived context replaced the
// command's own; both take twice the deadline to answer, and the login must
// still save the profile.
func TestAuthLoginPromptsOutliveTheTimeout(t *testing.T) {
	f := &fakeServer{}
	srv := f.start(t)
	dir := t.TempDir()
	setRdshEnv(t, dir, nil)

	const timeout = 200 * time.Millisecond
	stdin := &slowStdin{
		answers: authLoginAnswers(srv.URL),
		// The profile name prompt, which runs before the verification, and
		// the data source prompt, which runs after it.
		delayed: map[int]time.Duration{2: 2 * timeout, 3: 2 * timeout},
	}

	out, err := runRdshWithStdin(t, context.Background(), stdin, "auth", "login", "--timeout", timeout.String())
	if err != nil {
		t.Fatalf("auth login error = %v (output: %s)", err, out)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if _, ok := cfg.Profiles["staging"]; !ok {
		t.Errorf("profiles = %v, want the answered profile saved", cfg.Profiles)
	}
}
