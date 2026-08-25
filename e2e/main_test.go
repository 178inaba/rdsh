//go:build e2e

// Package e2e drives the rdsh binary against a real Redash.
//
// Every other test in this repository runs against an in-process fake, which
// encodes what rdsh believes Redash does — so a belief that is wrong is wrong
// in the code and in the fake at once, and the suite still passes. These tests
// cover only what a fake can be wrong about: the Redash-side contracts rdsh
// takes for granted. rdsh's own behaviour stays with the fake tests.
//
// The caller brings the sandbox up (`eval "$(scripts/redash-up.sh)"`); the
// tests know nothing about Docker and read RDSH_URL and RDSH_API_KEY.
package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The environment pair scripts/redash-up.sh prints, which is all rdsh needs
// to reach the sandbox.
const (
	urlEnv    = "RDSH_URL"
	apiKeyEnv = "RDSH_API_KEY"
)

// dataSource is what scripts/redash-up.sh registers over the seeded
// database. Authenticating through the environment pair leaves no profile to
// carry a default, so the two commands that take --data-source are passed it
// every time; query refresh and query update reject it as an unknown flag.
const dataSource = "sandbox"

var (
	// rdshPath is the binary every test execs, built once by TestMain.
	rdshPath string
	// configDir is an empty directory the runs use as XDG_CONFIG_HOME, so
	// that the developer's own config file cannot take part: the
	// environment pair outranks a profile, but resolveConnection loads the
	// file before that precedence is applied, and an unreadable one would
	// fail every run here for a reason that has nothing to do with Redash.
	configDir string

	redashURL string
	apiKey    string
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

// run is TestMain's body so that the build directory is still removed when
// the suite fails: os.Exit skips deferred calls.
func run(m *testing.M) int {
	redashURL, apiKey = os.Getenv(urlEnv), os.Getenv(apiKeyEnv)
	if redashURL == "" || apiKey == "" {
		// A failure rather than a skip. The e2e build tag means this run was
		// asked for, and a suite that quietly skips itself is a green lie
		// about contracts nothing else in the repository checks.
		fmt.Fprintf(os.Stderr, "e2e: %s and %s must both be set; "+
			"start the sandbox first with `eval \"$(scripts/redash-up.sh)\"`\n", urlEnv, apiKeyEnv)
		return 1
	}
	redashURL = strings.TrimRight(redashURL, "/")

	dir, err := os.MkdirTemp("", "rdsh-e2e")
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()

	// The package to build is the module root, which is this package's
	// parent — the tests exec the binary an agent would, not this package's
	// own code.
	rdshPath = filepath.Join(dir, "rdsh")
	build := exec.Command("go", "build", "-o", rdshPath, "..")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "e2e: building rdsh:", err)
		return 1
	}

	configDir = filepath.Join(dir, "config")
	return m.Run()
}

// result is one rdsh run as a caller of the CLI sees it. The exit code is
// part of it because agent consumers branch on it mechanically, and a
// subprocess is the only layer that observes it at all.
type result struct {
	stdout   string
	stderr   string
	exitCode int
}

// runRdsh execs the built binary and returns what it produced. A non-zero
// exit is not a failure here — several contracts below expect one — so only
// a binary that could not be started at all fails the test.
func runRdsh(t *testing.T, args ...string) result {
	t.Helper()

	cmd := exec.Command(rdshPath, args...)
	cmd.Env = append(os.Environ(),
		urlEnv+"="+redashURL,
		apiKeyEnv+"="+apiKey,
		"XDG_CONFIG_HOME="+configDir,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	err := cmd.Run()
	code := 0
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		code = exit.ExitCode()
	case err != nil:
		t.Fatalf("running rdsh %v: %v", args, err)
	}
	return result{stdout: stdout.String(), stderr: stderr.String(), exitCode: code}
}

// assertExit checks the code the run ended with, quoting stderr when it is
// not the expected one: the binary's own report is the only account of what
// went wrong, and a bare code mismatch is unreadable without it.
func (r result) assertExit(t *testing.T, code int) {
	t.Helper()
	if r.exitCode != code {
		t.Fatalf("rdsh exit code = %d, want %d; stderr = %q", r.exitCode, code, r.stderr)
	}
}

// uniqueName is a query name no earlier run can have used. The sandbox
// outlives a single `go test`, so tests sharing a name would read each
// other's queries back.
func uniqueName(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("rdsh e2e %s %d", t.Name(), time.Now().UnixNano())
}

// nonced returns sql carrying a comment no earlier run produced, so Redash
// hashes it to a query text it has never executed and no stored result can
// match it.
//
// The nonce has to be a line comment, and it has to differ in more than
// whitespace: gen_query_hash strips /* ... */ comments and collapses every
// space before hashing, so a block comment would leave the hash unchanged
// and the tests that turn on a query having no result yet would pass without
// checking anything. It goes on the first line rather than the last so that
// an appended LIMIT — what Redash's auto limit adds — cannot end up inside
// it.
func nonced(t *testing.T, sql string) string {
	t.Helper()
	return fmt.Sprintf("-- %s %d\n%s", t.Name(), time.Now().UnixNano(), sql)
}

// queryID reads the ID out of the URL `rdsh query create` prints, which is
// the only identifier the command reports.
func queryID(t *testing.T, stdout string) int {
	t.Helper()
	url := strings.TrimSpace(stdout)
	_, tail, ok := strings.Cut(url, "/queries/")
	if !ok {
		t.Fatalf("query create printed %q, want a URL under /queries/", url)
	}
	id, err := strconv.Atoi(tail)
	if err != nil {
		t.Fatalf("query create printed %q, whose ID does not parse: %v", url, err)
	}
	return id
}

// redashDo sends one authenticated request to Redash and decodes the JSON it
// answers with. The tests reach for it only where rdsh has no output for
// what is being asserted; driving rdsh's own work through it would defeat
// the point of exec'ing the binary.
//
// Numbers are kept as json.Number so that a stored default written as a
// number can be told from one written as a string.
func redashDo(t *testing.T, method, path string, body any) map[string]any {
	t.Helper()

	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encoding the %s %s body: %v", method, path, err)
		}
		reqBody = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, redashURL+path, reqBody)
	if err != nil {
		t.Fatalf("building the %s %s request: %v", method, path, err)
	}
	req.Header.Set("Authorization", "Key "+apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		t.Fatalf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, data)
	}
	decoder := json.NewDecoder(resp.Body)
	decoder.UseNumber()
	var out map[string]any
	if err := decoder.Decode(&out); err != nil {
		t.Fatalf("%s %s: decoding the response: %v", method, path, err)
	}
	return out
}

func queryPath(id int) string {
	return "/api/queries/" + strconv.Itoa(id)
}

// getQuery reads one saved query straight from Redash.
func getQuery(t *testing.T, id int) map[string]any {
	t.Helper()
	return redashDo(t, http.MethodGet, queryPath(id), nil)
}
