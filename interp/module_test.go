// Copyright (c) 2017, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

package interp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"mvdan.cc/sh/v3/syntax"
)

func blacklistBuiltin(name string) func(ExecModule) ExecModule {
	return ExecBuiltin(name, func(mc ModuleCtx, args []string) error {
		return fmt.Errorf("%s: blacklisted builtin", name)
	})
}

func blacklistExec(next ExecModule) ExecModule {
	return func(ctx context.Context, path string, args []string) error {
		return fmt.Errorf("blacklisted: %s", args[0])
	}
}

var modCases = []struct {
	name string
	exec func(ExecModule) ExecModule
	open func(OpenModule) OpenModule
	src  string
	want string
}{
	{
		name: "ExecBlacklist",
		exec: blacklistBuiltin("sleep"),
		src:  "echo foo; sleep 1",
		want: "foo\nsleep: blacklisted builtin",
	},
	{
		name: "ExecWhitelist",
		exec: blacklistBuiltin("faa"),
		src:  "a=$(echo foo | sed 's/o/a/g'); echo $a; $a args",
		want: "faa\nfaa: blacklisted builtin",
	},
	{
		name: "ExecSubshell",
		exec: blacklistExec,
		src:  "(malicious)",
		want: "blacklisted: malicious",
	},
	{
		name: "ExecPipe",
		exec: blacklistExec,
		src:  "malicious | echo foo",
		want: "foo\nblacklisted: malicious",
	},
	{
		name: "ExecCmdSubst",
		exec: blacklistExec,
		src:  "a=$(malicious)",
		want: "blacklisted: malicious\nexit status 1",
	},
	{
		name: "ExecBackground",
		exec: blacklistExec,
		src:  "{ malicious; true; } & { malicious; true; } & wait",
		want: "blacklisted: malicious",
	},
	{
		name: "OpenForbidNonDev",
		open: func(next OpenModule) OpenModule {
			return OpenDevImpls(func(ctx context.Context, path string, flags int, mode os.FileMode) (io.ReadWriteCloser, error) {
				mc, _ := FromModuleContext(ctx)
				return nil, fmt.Errorf("non-dev: %s", mc.UnixPath(path))
			})
		},
		src:  "echo foo >/dev/null; echo bar >/tmp/x",
		want: "non-dev: /tmp/x",
	},
}

func TestRunnerModules(t *testing.T) {
	t.Parallel()
	p := syntax.NewParser()
	for _, tc := range modCases {
		t.Run(tc.name, func(t *testing.T) {
			file := parse(t, p, tc.src)
			var cb concBuffer
			r, err := New(StdIO(nil, &cb, &cb))
			if tc.exec != nil {
				WithExecModules(tc.exec)(r)
			}
			if tc.open != nil {
				WithOpenModules(tc.open)(r)
			}
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if err := r.Run(ctx, file); err != nil {
				cb.WriteString(err.Error())
			}
			got := cb.String()
			if got != tc.want {
				t.Fatalf("want:\n%s\ngot:\n%s", tc.want, got)
			}
		})
	}
}

type readyBuffer struct {
	buf       bytes.Buffer
	seenReady sync.WaitGroup
}

func (b *readyBuffer) Write(p []byte) (n int, err error) {
	if string(p) == "ready\n" {
		b.seenReady.Done()
		return len(p), nil
	}
	return b.buf.Write(p)
}

func TestKillTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("sleeps and timeouts are slow")
	}
	if runtime.GOOS == "windows" {
		t.Skip("skipping trap tests on windows")
	}
	tests := []struct {
		src         string
		want        string
		killTimeout time.Duration
		forcedKill  bool
	}{
		// killed immediately
		{
			`bash -c "trap 'echo trapped; exit 0' INT; echo ready; for i in {1..100}; do sleep 0.01; done"`,
			"",
			-1,
			true,
		},
		// interrupted first, and stops itself in time
		{
			`bash -c "trap 'echo trapped; exit 0' INT; echo ready; for i in {1..100}; do sleep 0.01; done"`,
			"trapped\n",
			time.Second,
			false,
		},
		// interrupted first, but does not stop itself in time
		{
			`bash -c "trap 'echo trapped; for i in {1..100}; do sleep 0.01; done' INT; echo ready; for i in {1..100}; do sleep 0.01; done"`,
			"trapped\n",
			20 * time.Millisecond,
			true,
		},
	}

	for i := range tests {
		test := tests[i]
		t.Run(fmt.Sprintf("%d", i), func(t *testing.T) {
			t.Parallel()
			file := parse(t, nil, test.src)
			attempt := 0
			for {
				var rbuf readyBuffer
				rbuf.seenReady.Add(1)
				ctx, cancel := context.WithCancel(context.Background())
				r, err := New(StdIO(nil, &rbuf, &rbuf))
				if err != nil {
					t.Fatal(err)
				}
				r.KillTimeout = test.killTimeout
				go func() {
					rbuf.seenReady.Wait()
					cancel()
				}()
				err = r.Run(ctx, file)
				if test.forcedKill {
					if _, ok := err.(ExitStatus); ok || err == nil {
						t.Error("command was not force-killed")
					}
				} else {
					if err != nil && err != context.Canceled && err != context.DeadlineExceeded {
						t.Errorf("execution errored: %v", err)
					}
				}
				got := rbuf.buf.String()
				if got != test.want {
					if attempt < 3 && got == "" && test.killTimeout > 0 {
						attempt++
						test.killTimeout *= 2
						continue
					}
					t.Fatalf("want:\n%s\ngot:\n%s", test.want, got)
				}
				break
			}
		})
	}
}
