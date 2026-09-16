package guest

import (
	"context"
	"io"
	"strings"
	"sync"
)

// fakeExecutor records every script and answers from a table of canned
// responses keyed by a substring of the script.
type fakeExecutor struct {
	mu      sync.Mutex
	scripts []string
	stdins  []string

	// responses maps a script substring to what Run returns.
	responses map[string]fakeResponse
}

type fakeResponse struct {
	stdout string
	stderr string
	exit   int
}

func newFake() *fakeExecutor {
	return &fakeExecutor{responses: map[string]fakeResponse{}}
}

func (f *fakeExecutor) respond(substr string, r fakeResponse) {
	f.responses[substr] = r
}

func (f *fakeExecutor) lookup(script string) fakeResponse {
	for k, r := range f.responses {
		if strings.Contains(script, k) {
			return r
		}
	}

	return fakeResponse{}
}

func (f *fakeExecutor) record(script, stdin string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.scripts = append(f.scripts, script)
	f.stdins = append(f.stdins, stdin)
}

func (f *fakeExecutor) Run(ctx context.Context, script string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	f.record(script, "")

	r := f.lookup(script)
	if r.exit != 0 {
		return nil, &CommandError{Script: script, ExitCode: r.exit, Stderr: r.stderr}
	}

	return []byte(r.stdout), nil
}

func (f *fakeExecutor) Start(ctx context.Context, script string, io_ Streams) (Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var in []byte
	if io_.Stdin != nil {
		in, _ = io.ReadAll(io_.Stdin)
	}

	f.record(script, string(in))

	r := f.lookup(script)
	if io_.Stderr != nil && r.stderr != "" {
		_, _ = io.WriteString(io_.Stderr, r.stderr)
	}

	if io_.Stdout != nil && r.stdout != "" {
		_, _ = io.WriteString(io_.Stdout, r.stdout)
	}

	return fakeProcess{script: script, exit: r.exit}, nil
}

func (f *fakeExecutor) Shell(context.Context, Terminal) error { return nil }

type fakeProcess struct {
	script string
	exit   int
}

func (p fakeProcess) Wait() error {
	if p.exit != 0 {
		// Stderr deliberately left empty: feed() must fill it in.
		return &CommandError{Script: p.script, ExitCode: p.exit}
	}

	return nil
}
