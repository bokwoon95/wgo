package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"io/fs"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/creack/pty"
	"github.com/fsnotify/fsnotify"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

var WGO_RANDOM_NUMBER string

func init() {
	WGO_RANDOM_NUMBER = strconv.Itoa(rand.Intn(5000))
	os.Setenv("FOO", "green")
	os.Setenv("BAR", "lorem ipsum dolor sit amet")
	os.Setenv("WGO_RANDOM_NUMBER", WGO_RANDOM_NUMBER)
}

func Test_compileRegexp(t *testing.T) {
	type TestTable struct {
		description string
		pattern     string
		pass        []string
		fail        []string
	}

	tests := []TestTable{{
		description: "normal regexp without dot",
		pattern:     `ab\wd`,
		pass:        []string{"abcd", "abxd", "abzd"},
		fail:        []string{"ab@d", "ab.d"},
	}, {
		description: "dot followed by letter is treated as literal dot",
		pattern:     `.html`,
		pass:        []string{"header.html", "footer.html"},
		fail:        []string{"\\xhtml", "footer.xhtml", "main.go"},
	}, {
		description: "an escaped dot is not escaped again",
		pattern:     `\.html`,
		pass:        []string{"header.html", "footer.html"},
		fail:        []string{"\\xhtml", "footer.xhtml", "main.go"},
	}, {
		description: "dot followed by non-dot is treated as normal regexp dot",
		pattern:     `(.)html`,
		pass:        []string{"header.html", "footer.html", "\\xhtml", "footer.xhtml"},
		fail:        []string{"main.go"},
	}, {
		description: "trim patterns starting with dot slash",
		pattern:     `./testdata/hello_world/main.go`,
		pass:        []string{"testdata/hello_world/main.go"},
		fail:        []string{},
	}}

	for _, tt := range tests {
		t.Run(tt.description, func(t *testing.T) {
			t.Parallel()
			r, err := compileRegexp(tt.pattern)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range tt.pass {
				if !r.MatchString(s) {
					t.Errorf("%q failed to match %q", tt.pattern, s)
				}
			}
			for _, s := range tt.fail {
				if r.MatchString(s) {
					t.Errorf("%q incorrectly matches %q", tt.pattern, s)
				}
			}
		})
	}
}

func TestWgoCmd_match(t *testing.T) {
	type TestTable struct {
		description string
		roots       []string
		args        []string
		fileEvent   FileEvent
		want        bool
	}

	tests := []TestTable{{
		description: "-xfile",
		args:        []string{"-xfile", "_test.go"},
		fileEvent: FileEvent{
			Event: fsnotify.Event{
				Name: "wgo_cmd_test.go",
			},
		},
		want: false,
	}, {
		description: "-xfile with slash",
		args:        []string{"-xfile", "testdata/"},
		fileEvent: FileEvent{
			Event: fsnotify.Event{
				Name: "testdata/args/main.go",
			},
		},
		want: false,
	}, {
		description: "-file",
		args:        []string{"-file", "main.go"},
		fileEvent: FileEvent{
			Event: fsnotify.Event{
				Name: "testdata/args/main.go",
			},
		},
		want: true,
	}, {
		description: "-xdir overrides -file",
		args:        []string{"-file", "main.go", "-xdir", "testdata"},
		fileEvent: FileEvent{
			Event: fsnotify.Event{
				Name: "testdata/args/main.go",
			},
		},
		want: false,
	}, {
		description: "-file matches but -dir does not",
		args:        []string{"-file", "main.go", "-dir", "src"},
		fileEvent: FileEvent{
			Event: fsnotify.Event{
				Name: "testdata/args/main.go",
			},
		},
		want: false,
	}, {
		description: "both -file and -dir match",
		args:        []string{"-file", "main.go", "-dir", "testdata"},
		fileEvent: FileEvent{
			Event: fsnotify.Event{
				Name: "testdata/args/main.go",
			},
		},
		want: true,
	}, {
		description: "-file with slash",
		args:        []string{"-file", "testdata/"},
		fileEvent: FileEvent{
			Event: fsnotify.Event{
				Name: "testdata/args/main.go",
			},
		},
		want: true,
	}, {
		description: "wgo run",
		args:        []string{"run", "."},
		fileEvent: FileEvent{
			Event: fsnotify.Event{
				Name: "testdata/args/main.go",
			},
		},
		want: true,
	}, {
		description: "wgo run without flags exclude non go files",
		args:        []string{"run", "main.go"},
		fileEvent: FileEvent{
			Event: fsnotify.Event{
				Name: "testdata/dir/foo/bar.txt",
			},
		},
		want: false,
	}, {
		description: "fallthrough",
		args:        []string{"-file", ".go", "-file", "test", "-xfile", ".css", "-xfile", "assets"},
		fileEvent: FileEvent{
			Event: fsnotify.Event{
				Name: "index.html",
			},
		},
		want: false,
	}, {
		description: "root is truncated",
		roots:       []string{"/Documents"},
		args:        []string{"-file", "Documents"},
		fileEvent: FileEvent{
			Event: fsnotify.Event{
				Name: "/Documents/wgo/main.go",
			},
		},
		want: false,
	}, {
		description: "root is not truncated",
		roots:       []string{"/lorem_ipsum"},
		args:        []string{"-file", "Documents"},
		fileEvent: FileEvent{
			Event: fsnotify.Event{
				Name: "/Documents/wgo/main.go",
			},
		},
		want: true,
	}, {
		description: "nothing allows anything",
		args:        []string{},
		fileEvent: FileEvent{
			Event: fsnotify.Event{
				Name: "/Documents/index.rb",
			},
		},
		want: true,
	}, {
		description: "directory uses -dir and ignores -file",
		args:        []string{"-dir", "testdata/dir", "-xfile", "dir"},
		fileEvent: FileEvent{
			Event: fsnotify.Event{
				Name: "testdata/dir",
			},
			IsDir: true,
		},
		want: true,
	}, {
		description: "directory excluded by -xdir",
		args:        []string{"-xdir", "testdata/dir"},
		fileEvent: FileEvent{
			Event: fsnotify.Event{
				Name: "testdata/dir",
			},
			IsDir: true,
		},
		want: false,
	}, {
		description: "default excluded directory",
		args:        []string{},
		fileEvent: FileEvent{
			Event: fsnotify.Event{
				Name: "testdata/node_modules",
			},
			IsDir: true,
		},
		want: false,
	}, {
		description: "default excluded hidden directory",
		args:        []string{},
		fileEvent: FileEvent{
			Event: fsnotify.Event{
				Name: "testdata/.cache",
			},
			IsDir: true,
		},
		want: false,
	}, {
		description: "explicitly included default excluded directory",
		args:        []string{"-dir", "node_modules"},
		fileEvent: FileEvent{
			Event: fsnotify.Event{
				Name: "testdata/node_modules",
			},
			IsDir: true,
		},
		want: true,
	}}

	for _, tt := range tests {
		t.Run(tt.description, func(t *testing.T) {
			t.Parallel()
			wgoCmd, err := WgoCommand(context.Background(), 0, tt.args)
			if err != nil {
				t.Fatal(err)
			}
			if tt.roots != nil {
				wgoCmd.Roots = make([]string, len(tt.roots))
				for i := range tt.roots {
					wgoCmd.Roots[i], err = filepath.Abs(tt.roots[i])
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			tt.fileEvent.Name, err = filepath.Abs(tt.fileEvent.Name)
			if err != nil {
				t.Fatal(err)
			}
			got := wgoCmd.match(tt.fileEvent)
			if !got && tt.want {
				t.Errorf("%v failed to match %q", tt.args, tt.fileEvent.Name)
			} else if got && !tt.want {
				t.Errorf("%v incorrectly matches %q", tt.args, tt.fileEvent.Name)
			}
		})
	}
}

func TestWgoCmd_addDirsRecursively(t *testing.T) {
	type TestTable struct {
		description string
		roots       []string
		dir         string
		args        []string
		wantWatched []string
	}

	// NOTE: Don't hardcode absolute paths here, use only relative paths. The
	// test scaffolding will convert them to absolute paths for you.
	tests := []TestTable{{
		description: "-xdir",
		roots:       []string{"testdata/dir"},
		dir:         "testdata/dir",
		args:        []string{"-xdir", "subdir"},
		wantWatched: []string{
			"testdata/dir",
			"testdata/dir/foo",
		},
	}, {
		description: "-xdir with slash",
		roots:       []string{"testdata/dir"},
		dir:         "testdata/dir",
		args:        []string{"-xdir", "/"},
		wantWatched: []string{
			"testdata/dir",
		},
	}, {
		description: "-xdir excludes non root dir",
		args:        []string{"-xdir", "testdata/dir"},
		dir:         "testdata/dir",
		wantWatched: []string{},
	}, {
		description: "-dir",
		roots:       []string{"testdata/dir"},
		dir:         "testdata/dir",
		args:        []string{"-dir", "foo"},
		wantWatched: []string{
			"testdata/dir",
			"testdata/dir/foo",
			"testdata/dir/subdir",
			"testdata/dir/subdir/foo",
		},
	}, {
		description: "explicitly include node_modules",
		roots:       []string{"testdata/dir"},
		dir:         "testdata/dir",
		args:        []string{"-dir", "node_modules"},
		wantWatched: []string{
			"testdata/dir",
			"testdata/dir/foo",
			"testdata/dir/node_modules",
			"testdata/dir/node_modules/foo",
			"testdata/dir/subdir",
			"testdata/dir/subdir/foo",
		},
	}}

	for _, tt := range tests {
		t.Run(tt.description, func(t *testing.T) {
			t.Parallel()
			wgoCmd, err := WgoCommand(context.Background(), 0, tt.args)
			if err != nil {
				t.Fatal(err)
			}
			for i := range tt.roots {
				root, err := filepath.Abs(tt.roots[i])
				if err != nil {
					t.Fatal(err)
				}
				wgoCmd.Roots = append(wgoCmd.Roots, root)
			}
			watcher, err := fsnotify.NewWatcher()
			if err != nil {
				t.Fatal(err)
			}
			dir, err := filepath.Abs(tt.dir)
			if err != nil {
				t.Fatal(err)
			}
			for i := range tt.wantWatched {
				tt.wantWatched[i], err = filepath.Abs(tt.wantWatched[i])
				if err != nil {
					t.Fatal(err)
				}
			}
			dirSet := make(map[string]struct{})
			wgoCmd.addDirsRecursively(watcher, dir, dirSet)
			gotWatched := watcher.WatchList()
			sort.Strings(gotWatched)
			sort.Strings(tt.wantWatched)
			if diff := Diff(gotWatched, tt.wantWatched); diff != "" {
				t.Error(diff)
			}
		})
	}
}

func TestWgoCmd_newPolledDirectory(t *testing.T) {
	t.Parallel()

	testDir := t.TempDir()
	for _, name := range []string{"excluded", "node_modules", ".git", ".hidden", "normal"} {
		err := os.Mkdir(filepath.Join(testDir, name), 0777)
		if err != nil {
			t.Fatal(err)
		}
	}

	wgoCmd, err := WgoCommand(context.Background(), 0, []string{
		"-xdir", "excluded", "-dir", "node_modules",
	})
	if err != nil {
		t.Fatal(err)
	}
	wgoCmd.Roots = []string{testDir}
	wgoCmd.Logger = log.New(io.Discard, "", 0)

	polledFile, err := wgoCmd.newPolledDirectory(testDir)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(polledFile.ChildMap))
	for name := range polledFile.ChildMap {
		got = append(got, name)
	}
	sort.Strings(got)
	want := []string{"node_modules", "normal"}
	if diff := Diff(got, want); diff != "" {
		t.Error(diff)
	}
}

func TestWgoCommands(t *testing.T) {
	type TestTable struct {
		description string
		args        []string
		wantCmds    []*WgoCmd
	}

	tests := []TestTable{{
		description: "chained commands",
		args: []string{
			"wgo", "-file", ".go", "clear",
			"::", "echo", "building...",
			"::", "go", "build", "-o", "hello_world", "hello_world.go",
			"::", "echo", "running...",
			"::", "./hello_world",
		},
		wantCmds: []*WgoCmd{{
			Roots:       []string{"."},
			FileRegexps: []*regexp.Regexp{regexp.MustCompile(`\.go`)},
			ArgsList: [][]string{
				{"clear"},
				{"echo", "building..."},
				{"go", "build", "-o", "hello_world", "hello_world.go"},
				{"echo", "running..."},
				{"./hello_world"},
			},
			Debounce: 300 * time.Millisecond,
		}},
	}, {
		description: "parallel commands",
		args: []string{
			"wgo", "run", "-tags", "fts5", "main.go", "arg1", "arg2",
			"::", "wgo", "-file", ".css", "-dir", "assets", "sass", "assets/styles.scss", "assets/styles.css",
			"::", "wgo", "-file", ".js", "-dir", "assets", "tsc", "assets/*.ts", "--outfile", "assets/index.js",
		},
		wantCmds: []*WgoCmd{{
			Roots: []string{"."},
			ArgsList: [][]string{
				{"go", "build", "-o", "out", "-tags", "fts5", "main.go"},
				{"out", "arg1", "arg2"},
			},
			Debounce:       300 * time.Millisecond,
			isRun:          true,
			executablePath: "out",
		}, {
			Roots:       []string{"."},
			FileRegexps: []*regexp.Regexp{regexp.MustCompile(`\.css`)},
			DirRegexps:  []*regexp.Regexp{regexp.MustCompile(`assets`)},
			ArgsList: [][]string{
				{"sass", "assets/styles.scss", "assets/styles.css"},
			},
			Debounce: 300 * time.Millisecond,
		}, {
			Roots:       []string{"."},
			FileRegexps: []*regexp.Regexp{regexp.MustCompile(`\.js`)},
			DirRegexps:  []*regexp.Regexp{regexp.MustCompile(`assets`)},
			ArgsList: [][]string{
				{"tsc", "assets/*.ts", "--outfile", "assets/index.js"},
			},
			Debounce: 300 * time.Millisecond,
		}},
	}, {
		description: "build flags",
		args: []string{
			"wgo", "run", "-a", "-n", "-race", "-msan", "-asan", "-v=false",
			"-work", "-x", "-buildvcs", "-linkshared=true", "-modcacherw=1",
			"-trimpath=t", "-p", "5", ".", "arg1", "arg2",
		},
		wantCmds: []*WgoCmd{{
			Roots: []string{"."},
			ArgsList: [][]string{
				{"go", "build", "-o", "out", "-p", "5", "-a", "-n", "-race", "-msan", "-asan", "-work", "-x", "-buildvcs", "-linkshared", "-modcacherw", "-trimpath", "."},
				{"out", "arg1", "arg2"},
			},
			Debounce:       300 * time.Millisecond,
			isRun:          true,
			executablePath: "out",
		}},
	}, {
		description: "wgo flags",
		args: []string{
			"wgo", "-root", "/secrets", "-file", ".", "-verbose", "-postpone", "echo", "hello",
		},
		wantCmds: []*WgoCmd{{
			Roots:       []string{".", "/secrets"},
			FileRegexps: []*regexp.Regexp{regexp.MustCompile(`.`)},
			ArgsList: [][]string{
				{"echo", "hello"},
			},
			Debounce: 300 * time.Millisecond,
			Postpone: true,
		}},
	}, {
		description: "escaped ::",
		args: []string{
			"wgo", "-file", ".", "echo", ":::", "::::", ":::::",
		},
		wantCmds: []*WgoCmd{{
			Roots:       []string{"."},
			FileRegexps: []*regexp.Regexp{regexp.MustCompile(`.`)},
			ArgsList: [][]string{
				{"echo", "::", ":::", "::::"},
			},
			Debounce: 300 * time.Millisecond,
		}},
	}, {
		description: "debounce flag",
		args: []string{
			"wgo", "-debounce", "10ms", "echo", "test",
		},
		wantCmds: []*WgoCmd{{
			Roots: []string{"."},
			ArgsList: [][]string{
				{"echo", "test"},
			},
			Debounce: 10 * time.Millisecond,
		}},
	}}

	for _, tt := range tests {
		t.Run(tt.description, func(t *testing.T) {
			t.Parallel()
			gotCmds, err := WgoCommands(context.Background(), tt.args)
			if err != nil {
				t.Fatal(err)
			}
			for _, wgoCmd := range tt.wantCmds {
				wgoCmd.ctx = context.Background()
				for i := range wgoCmd.Roots {
					wgoCmd.Roots[i], err = filepath.Abs(wgoCmd.Roots[i])
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			// This is ugly, but because the executablePath is randomly
			// generated we have to manually reach into the argslist and
			// overwrite it with a well-known string so that we can compare the
			// commands properly.
			if tt.description == "parallel commands" || tt.description == "build flags" {
				gotCmds[0].executablePath = "out"
				gotCmds[0].ArgsList[0][3] = "out"
				gotCmds[0].ArgsList[1][0] = "out"
			}
			opts := []cmp.Option{
				// These fields should be excluded from comparison.
				cmpopts.IgnoreFields(WgoCmd{}, "Logger"),
				cmpopts.IgnoreFields(WgoCmd{}, "ErrorLogger"),
				cmpopts.IgnoreFields(WgoCmd{}, "Stdin"),
				cmpopts.IgnoreFields(WgoCmd{}, "Stdout"),
				cmpopts.IgnoreFields(WgoCmd{}, "Stderr"),
			}
			if diff := Diff(gotCmds, tt.wantCmds, opts...); diff != "" {
				t.Error(diff)
			}
		})
	}
}

func TestWgoCmd_Run(t *testing.T) {
	t.Run("args", func(t *testing.T) {
		t.Parallel()
		wgoCmd, err := WgoCommand(context.Background(), 0, []string{
			"run", "-exit", "-dir", "testdata/args", "./testdata/args", "apple", "banana", "cherry",
		})
		if err != nil {
			t.Fatal(err)
		}
		buf := &Buffer{}
		wgoCmd.Stdout = buf
		err = wgoCmd.Run()
		if err != nil {
			t.Fatal(err)
		}
		got := strings.TrimSpace(buf.String())
		want := "[apple banana cherry]"
		if got != want {
			t.Errorf("\ngot:  %q\nwant: %q", got, want)
		}
	})

	t.Run("build flags off", func(t *testing.T) {
		t.Parallel()
		wgoCmd, err := WgoCommand(context.Background(), 0, []string{
			"run", "-exit", "-dir", "testdata/build_flags", "./testdata/build_flags",
		})
		if err != nil {
			t.Fatal(err)
		}
		buf := &Buffer{}
		wgoCmd.Stdout = buf
		err = wgoCmd.Run()
		if err != nil {
			t.Fatal(err)
		}
		got := strings.TrimSpace(buf.String())
		want := "[foo]"
		if got != want {
			t.Errorf("\ngot:  %q\nwant: %q", got, want)
		}
	})

	t.Run("build flags on", func(t *testing.T) {
		t.Parallel()
		wgoCmd, err := WgoCommand(context.Background(), 0, []string{
			"run", "-exit", "-dir", "testdata/build_flags", "-tags=bar", "./testdata/build_flags",
		})
		if err != nil {
			t.Fatal(err)
		}
		buf := &Buffer{}
		wgoCmd.Stdout = buf
		err = wgoCmd.Run()
		if err != nil {
			t.Fatal(err)
		}
		got := strings.TrimSpace(buf.String())
		want := "[foo bar]"
		if got != want {
			t.Errorf("\ngot:  %q\nwant: %q", got, want)
		}
	})

	t.Run("env", func(t *testing.T) {
		t.Parallel()
		cmd, err := WgoCommand(context.Background(), 0, []string{
			"run", "-exit", "-dir", "testdata/env", "./testdata/env",
		})
		if err != nil {
			t.Fatal(err)
		}
		buf := &Buffer{}
		cmd.Stdout = buf
		err = cmd.Run()
		if err != nil {
			t.Fatal(err)
		}
		got := strings.TrimSpace(buf.String())
		want := `FOO=green
BAR=lorem ipsum dolor sit amet
WGO_RANDOM_NUMBER=` + WGO_RANDOM_NUMBER
		if got != want {
			t.Fatalf("\ngot:  %q\nwant: %q", got, want)
		}
	})

	t.Run("timeout off", func(t *testing.T) {
		t.Parallel()
		executablePath := filepath.Join(t.TempDir(), "timeout_off.exe")
		wgoCmd, err := WgoCommand(context.Background(), 0, []string{
			"-exit", "-dir", "testdata/hello_world", "-file", ".go", "go", "build", "-o", executablePath, "./testdata/hello_world",
			"::", executablePath,
		})
		if err != nil {
			t.Fatal(err)
		}
		buf := &Buffer{}
		wgoCmd.Stdout = buf
		err = wgoCmd.Run()
		if err != nil {
			t.Fatal(err)
		}
		got := strings.TrimSpace(buf.String())
		want := "hello world"
		if got != want {
			t.Errorf("\ngot:  %q\nwant: %q", got, want)
		}
	})

	t.Run("timeout on", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 0)
		defer cancel()
		executablePath := filepath.Join(t.TempDir(), "timeout_on.exe")
		wgoCmd, err := WgoCommand(ctx, 0, []string{
			"-exit", "-dir", "testdata/hello_world", "-file", ".go", "go", "build", "-o", executablePath, "./testdata/hello_world",
			"::", executablePath,
		})
		if err != nil {
			t.Fatal(err)
		}
		buf := &Buffer{}
		wgoCmd.Stdout = buf
		err = wgoCmd.Run()
		if err != nil {
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
		} else {
			t.Errorf("\ngot:  nil error\nwant: context.DeadlineExceeded")
		}
	})

	t.Run("signal off", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		wgoCmd, err := WgoCommand(ctx, 0, []string{
			"run", "-dir", "testdata/signal", "./testdata/signal",
		})
		if err != nil {
			t.Fatal(err)
		}
		stdoutBuffer := &Buffer{
			written: make(chan struct{}, 1),
		}
		wgoCmd.Stdout = stdoutBuffer
		cmdResult := make(chan error, 1)
		go func() {
			cmdResult <- wgoCmd.Run()
		}()

		timer := time.NewTimer(time.Minute)
		defer timer.Stop()
		want := "Waiting..."
		for stdoutBuffer.Len() < len(want) {
			select {
			case <-stdoutBuffer.written:
			case err := <-cmdResult:
				t.Fatalf("command exited before producing expected output: %v", err)
			case <-timer.C:
				t.Fatalf("timed out waiting for output:\n%v", Diff(strings.TrimSpace(stdoutBuffer.String()), want))
			}
		}
		if diff := Diff(strings.TrimSpace(stdoutBuffer.String()), want); diff != "" {
			t.Fatal(diff)
		}

		cancel()
		err = <-cmdResult
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})

	t.Run("signal on", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Windows doesn't support sending signals to a running process, skipping.")
		}
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		wgoCmd, err := WgoCommand(ctx, 0, []string{
			"run", "-dir", "testdata/signal", "./testdata/signal", "-trap-signal",
		})
		if err != nil {
			t.Fatal(err)
		}
		stdoutBuffer := &Buffer{
			written: make(chan struct{}, 1),
		}
		wgoCmd.Stdout = stdoutBuffer
		cmdResult := make(chan error, 1)
		go func() {
			cmdResult <- wgoCmd.Run()
		}()

		timer := time.NewTimer(time.Minute)
		defer timer.Stop()
		want := "Waiting..."
		for stdoutBuffer.Len() < len(want) {
			select {
			case <-stdoutBuffer.written:
			case err := <-cmdResult:
				t.Fatalf("command exited before it was ready to receive a signal: %v", err)
			case <-timer.C:
				t.Fatalf("timed out waiting for output:\n%v", Diff(strings.TrimSpace(stdoutBuffer.String()), want))
			}
		}
		if diff := Diff(strings.TrimSpace(stdoutBuffer.String()), want); diff != "" {
			t.Fatal(diff)
		}

		cancel()
		want = `Waiting...
Interrupt received, graceful shutdown.`
		for stdoutBuffer.Len() < len(want) {
			select {
			case <-stdoutBuffer.written:
			case err := <-cmdResult:
				t.Fatalf("command exited before producing expected output: %v", err)
			case <-timer.C:
				t.Fatalf("timed out waiting for output:\n%v", Diff(strings.TrimSpace(stdoutBuffer.String()), want))
			}
		}
		if diff := Diff(strings.TrimSpace(stdoutBuffer.String()), want); diff != "" {
			t.Fatal(diff)
		}

		err = <-cmdResult
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})

	t.Run("postpone off", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		testDir := t.TempDir()
		wgoCmd, err := WgoCommand(ctx, 0, []string{
			"-file", ".txt", "echo", "hello",
		})
		if err != nil {
			t.Fatal(err)
		}
		wgoCmd.Roots = []string{testDir}
		stdoutBuffer := &Buffer{
			written: make(chan struct{}, 1),
		}
		wgoCmd.Stdout = stdoutBuffer
		cmdResult := make(chan error, 1)
		go func() {
			cmdResult <- wgoCmd.Run()
		}()

		timer := time.NewTimer(time.Minute)
		defer timer.Stop()
		want := "hello"
		for stdoutBuffer.Len() < len(want) {
			select {
			case <-stdoutBuffer.written:
			case err := <-cmdResult:
				t.Fatalf("command exited before producing expected output: %v", err)
			case <-timer.C:
				t.Fatalf("timed out waiting for output:\n%v", Diff(strings.TrimSpace(stdoutBuffer.String()), want))
			}
		}
		if diff := Diff(strings.TrimSpace(stdoutBuffer.String()), want); diff != "" {
			t.Fatal(diff)
		}

		err = os.WriteFile(filepath.Join(testDir, "trigger.txt"), []byte("trigger"), 0666)
		if err != nil {
			t.Fatal(err)
		}
		want = "hello\nhello"
		for stdoutBuffer.Len() < len(want) {
			select {
			case <-stdoutBuffer.written:
			case err := <-cmdResult:
				t.Fatalf("command exited before producing expected output: %v", err)
			case <-timer.C:
				t.Fatalf("timed out waiting for output:\n%v", Diff(strings.TrimSpace(stdoutBuffer.String()), want))
			}
		}
		if diff := Diff(strings.TrimSpace(stdoutBuffer.String()), want); diff != "" {
			t.Fatal(diff)
		}

		cancel()
		err = <-cmdResult
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})

	t.Run("postpone on", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		testDir := t.TempDir()
		wgoCmd, err := WgoCommand(ctx, 0, []string{
			"-file", ".txt", "-postpone", "echo", "hello",
		})
		if err != nil {
			t.Fatal(err)
		}
		wgoCmd.Roots = []string{testDir}
		loggerBuffer := &Buffer{
			written: make(chan struct{}, 1),
		}
		wgoCmd.Logger = log.New(loggerBuffer, "", 0)
		stdoutBuffer := &Buffer{
			written: make(chan struct{}, 1),
		}
		wgoCmd.Stdout = stdoutBuffer
		cmdResult := make(chan error, 1)
		go func() {
			cmdResult <- wgoCmd.Run()
		}()

		// Wait for script to start (use logger output as the signal).
		timer := time.NewTimer(time.Minute)
		defer timer.Stop()
		for loggerBuffer.Len() == 0 {
			select {
			case <-loggerBuffer.written:
			case err = <-cmdResult:
				t.Fatalf("command exited before starting: %v", err)
			case <-timer.C:
				t.Fatal("timed out waiting for command to start")
			}
		}

		want := ""
		for stdoutBuffer.Len() < len(want) {
			select {
			case <-stdoutBuffer.written:
			case err := <-cmdResult:
				t.Fatalf("command exited before producing expected output: %v", err)
			case <-timer.C:
				t.Fatalf("timed out waiting for output:\n%v", Diff(strings.TrimSpace(stdoutBuffer.String()), want))
			}
		}
		if diff := Diff(strings.TrimSpace(stdoutBuffer.String()), want); diff != "" {
			t.Fatal(diff)
		}

		err = os.WriteFile(filepath.Join(testDir, "trigger.txt"), []byte("trigger"), 0666)
		if err != nil {
			t.Fatal(err)
		}
		want = "hello"
		for stdoutBuffer.Len() < len(want) {
			select {
			case <-stdoutBuffer.written:
			case err := <-cmdResult:
				t.Fatalf("command exited before producing expected output: %v", err)
			case <-timer.C:
				t.Fatalf("timed out waiting for output:\n%v", Diff(strings.TrimSpace(stdoutBuffer.String()), want))
			}
		}
		if diff := Diff(strings.TrimSpace(stdoutBuffer.String()), want); diff != "" {
			t.Fatal(diff)
		}

		cancel()
		err = <-cmdResult
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
}

func TestWgoCmd_FileEvent(t *testing.T) {
	type TestTable struct {
		description string
		fsys        fs.FS
		changes     func(testDir string) error
		want        string
	}
	t.Parallel()

	tests := []TestTable{{
		description: "add file",
		fsys: fstest.MapFS{
			"alpha.md":      {Data: []byte("alpha")},
			"data/base.bin": {Data: []byte{0x01, 0x02, 0x03}},
		},
		changes: func(testDir string) error {
			return os.WriteFile(filepath.Join(testDir, "added.txt"), []byte("added"), 0666)
		},
		want: `---
alpha.md: alpha
data/base.bin
---
added.txt: added
alpha.md: alpha
data/base.bin`,
	}, {
		description: "edit file",
		fsys: fstest.MapFS{
			"config.txt": {Data: []byte("before")},
			"notes.md":   {Data: []byte("notes")},
		},
		changes: func(testDir string) error {
			return os.WriteFile(filepath.Join(testDir, "config.txt"), []byte("after"), 0666)
		},
		want: `---
config.txt: before
notes.md: notes
---
config.txt: after
notes.md: notes`,
	}, {
		description: "create nested directory",
		fsys: fstest.MapFS{
			"root.txt":       {Data: []byte("root")},
			"static/keep.md": {Data: []byte("keep")},
		},
		changes: func(testDir string) error {
			err := os.MkdirAll(filepath.Join(testDir, "internal", "nested"), 0777)
			if err != nil {
				return err
			}
			err = os.WriteFile(filepath.Join(testDir, "internal", "bar.txt"), []byte("bar"), 0666)
			if err != nil {
				return err
			}
			err = os.WriteFile(filepath.Join(testDir, "internal", "nested", "baz.txt"), []byte("baz"), 0666)
			if err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(testDir, "root.txt"), []byte("root updated"), 0666)
		},
		want: `---
root.txt: root
static/keep.md: keep
---
internal/bar.txt: bar
internal/nested/baz.txt: baz
root.txt: root updated
static/keep.md: keep`,
	}, {
		description: "remove file",
		fsys: fstest.MapFS{
			"keep.md":    {Data: []byte("keep")},
			"target.txt": {Data: []byte("target")},
		},
		changes: func(testDir string) error {
			return os.Remove(filepath.Join(testDir, "target.txt"))
		},
		want: `---
keep.md: keep
target.txt: target
---
keep.md: keep`,
	}, {
		description: "rename file",
		fsys: fstest.MapFS{
			"keep.md":    {Data: []byte("keep")},
			"target.txt": {Data: []byte("target")},
		},
		changes: func(testDir string) error {
			return os.Rename(filepath.Join(testDir, "target.txt"), filepath.Join(testDir, "target.md"))
		},
		want: `---
keep.md: keep
target.txt: target
---
keep.md: keep
target.md: target`,
	}, {
		description: "rename directory",
		fsys: fstest.MapFS{
			"keep.md":                  {Data: []byte("keep")},
			"target/child.txt":         {Data: []byte("child")},
			"target/nested/grandchild": {Data: []byte("grandchild")},
		},
		changes: func(testDir string) error {
			return os.Rename(filepath.Join(testDir, "target"), filepath.Join(testDir, "renamed"))
		},
		want: `---
keep.md: keep
target/child.txt: child
target/nested/grandchild
---
keep.md: keep
renamed/child.txt: child
renamed/nested/grandchild`,
	}, {
		description: "replace file with directory",
		fsys: fstest.MapFS{
			"keep.md":    {Data: []byte("keep")},
			"target.txt": {Data: []byte("target")},
		},
		changes: func(testDir string) error {
			err := os.Remove(filepath.Join(testDir, "target.txt"))
			if err != nil {
				return err
			}
			err = os.Mkdir(filepath.Join(testDir, "target.txt"), 0777)
			if err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(testDir, "target.txt", "child.txt"), []byte("child"), 0666)
		},
		want: `---
keep.md: keep
target.txt: target
---
keep.md: keep
target.txt/child.txt: child`,
	}}

	// Precompile the testdata/file_event executable.
	executablePath := filepath.Join(t.TempDir(), "file_event.exe")
	err := exec.Command("go", "build", "-o", executablePath, "./testdata/file_event").Run()
	if err != nil {
		t.Fatal(err)
	}

	for _, tt := range tests {
		t.Run(tt.description, func(t *testing.T) {
			t.Parallel()

			// Initialize the directory.
			testDir := t.TempDir()
			err := fs.WalkDir(tt.fsys, ".", func(path string, dirEntry fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if path == "." {
					return nil
				}
				destPath := filepath.Join(testDir, filepath.FromSlash(path))
				if dirEntry.IsDir() {
					return os.MkdirAll(destPath, 0777)
				}
				contents, err := fs.ReadFile(tt.fsys, path)
				if err != nil {
					return err
				}
				return os.WriteFile(destPath, contents, 0666)
			})
			if err != nil {
				t.Fatal(err)
			}

			// Start the wgoCmd.
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			wgoCmd, err := WgoCommand(ctx, 0, []string{"-file", ".txt", "-verbose", executablePath, testDir})
			if err != nil {
				t.Fatal(err)
			}
			wgoCmd.Roots = []string{testDir}
			stdoutBuffer := &Buffer{
				written: make(chan struct{}, 1),
			}
			wgoCmd.Stdout = stdoutBuffer
			cmdResult := make(chan error, 1)
			go func() {
				cmdResult <- wgoCmd.Run()
			}()

			// Wait for initial script output.
			timer := time.NewTimer(time.Minute)
			defer timer.Stop()
			select {
			case <-stdoutBuffer.written:
			case err := <-cmdResult:
				t.Fatalf("command exited before producing initial output: %v", err)
			case <-timer.C:
				t.Fatal("timed out waiting for initial output")
			}

			// Make changes.
			err = tt.changes(testDir)
			if err != nil {
				t.Fatal(err)
			}

			// Assert the changes were reflected.
			for stdoutBuffer.Len() < len(tt.want) {
				select {
				case <-stdoutBuffer.written:
				case err := <-cmdResult:
					t.Fatalf("command exited before producing expected output: %v", err)
				case <-timer.C:
					t.Fatalf("timed out waiting for output:\n%v", Diff(strings.TrimSpace(stdoutBuffer.String()), tt.want))
				}
			}
			if diff := Diff(strings.TrimSpace(stdoutBuffer.String()), tt.want); diff != "" {
				t.Fatal(diff)
			}

			// Shut down the wgoCmd.
			cancel()
			err = <-cmdResult
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	}
}

func TestWgoCmd_Polling(t *testing.T) {
	type TestTable struct {
		description string
		fsys        fs.FS
		changes     func(testDir string) error
		want        string
	}
	t.Parallel()

	tests := []TestTable{{
		description: "add file",
		fsys: fstest.MapFS{
			"alpha.md":      {Data: []byte("alpha")},
			"data/base.bin": {Data: []byte{0x01, 0x02, 0x03}},
		},
		changes: func(testDir string) error {
			return os.WriteFile(filepath.Join(testDir, "added.txt"), []byte("added"), 0666)
		},
		want: `---
alpha.md: alpha
data/base.bin
---
added.txt: added
alpha.md: alpha
data/base.bin`,
	}, {
		description: "edit file",
		fsys: fstest.MapFS{
			"config.txt": {Data: []byte("before")},
			"notes.md":   {Data: []byte("notes")},
		},
		changes: func(testDir string) error {
			return os.WriteFile(filepath.Join(testDir, "config.txt"), []byte("after"), 0666)
		},
		want: `---
config.txt: before
notes.md: notes
---
config.txt: after
notes.md: notes`,
	}, {
		description: "create nested directory",
		fsys: fstest.MapFS{
			"root.txt":       {Data: []byte("root")},
			"static/keep.md": {Data: []byte("keep")},
		},
		changes: func(testDir string) error {
			err := os.MkdirAll(filepath.Join(testDir, "internal", "nested"), 0777)
			if err != nil {
				return err
			}
			err = os.WriteFile(filepath.Join(testDir, "internal", "bar.txt"), []byte("bar"), 0666)
			if err != nil {
				return err
			}
			err = os.WriteFile(filepath.Join(testDir, "internal", "nested", "baz.txt"), []byte("baz"), 0666)
			if err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(testDir, "root.txt"), []byte("root updated"), 0666)
		},
		want: `---
root.txt: root
static/keep.md: keep
---
internal/bar.txt: bar
internal/nested/baz.txt: baz
root.txt: root updated
static/keep.md: keep`,
	}, {
		description: "remove file",
		fsys: fstest.MapFS{
			"keep.md":    {Data: []byte("keep")},
			"target.txt": {Data: []byte("target")},
		},
		changes: func(testDir string) error {
			return os.Remove(filepath.Join(testDir, "target.txt"))
		},
		want: `---
keep.md: keep
target.txt: target
---
keep.md: keep`,
	}, {
		description: "rename file",
		fsys: fstest.MapFS{
			"keep.md":    {Data: []byte("keep")},
			"target.txt": {Data: []byte("target")},
		},
		changes: func(testDir string) error {
			return os.Rename(filepath.Join(testDir, "target.txt"), filepath.Join(testDir, "target.md"))
		},
		want: `---
keep.md: keep
target.txt: target
---
keep.md: keep
target.md: target`,
	}, {
		description: "rename directory",
		fsys: fstest.MapFS{
			"keep.md":                  {Data: []byte("keep")},
			"target/child.txt":         {Data: []byte("child")},
			"target/nested/grandchild": {Data: []byte("grandchild")},
		},
		changes: func(testDir string) error {
			return os.Rename(filepath.Join(testDir, "target"), filepath.Join(testDir, "renamed"))
		},
		want: `---
keep.md: keep
target/child.txt: child
target/nested/grandchild
---
keep.md: keep
renamed/child.txt: child
renamed/nested/grandchild`,
	}, {
		description: "replace file with directory",
		fsys: fstest.MapFS{
			"keep.md":    {Data: []byte("keep")},
			"target.txt": {Data: []byte("target")},
		},
		changes: func(testDir string) error {
			err := os.Remove(filepath.Join(testDir, "target.txt"))
			if err != nil {
				return err
			}
			err = os.Mkdir(filepath.Join(testDir, "target.txt"), 0777)
			if err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(testDir, "target.txt", "child.txt"), []byte("child"), 0666)
		},
		want: `---
keep.md: keep
target.txt: target
---
keep.md: keep
target.txt/child.txt: child`,
	}}

	// Precompile the testdata/polling executable.
	executablePath := filepath.Join(t.TempDir(), "polling.exe")
	err := exec.Command("go", "build", "-o", executablePath, "./testdata/polling").Run()
	if err != nil {
		t.Fatal(err)
	}

	for _, tt := range tests {
		t.Run(tt.description, func(t *testing.T) {
			t.Parallel()

			// Initialize the directory.
			testDir := t.TempDir()
			err := fs.WalkDir(tt.fsys, ".", func(path string, dirEntry fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if path == "." {
					return nil
				}
				destPath := filepath.Join(testDir, filepath.FromSlash(path))
				if dirEntry.IsDir() {
					return os.MkdirAll(destPath, 0777)
				}
				contents, err := fs.ReadFile(tt.fsys, path)
				if err != nil {
					return err
				}
				return os.WriteFile(destPath, contents, 0666)
			})
			if err != nil {
				t.Fatal(err)
			}

			// Start the wgoCmd.
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			wgoCmd, err := WgoCommand(ctx, 0, []string{"-file", ".txt", "-poll", "100ms", "-verbose", executablePath, testDir})
			if err != nil {
				t.Fatal(err)
			}
			wgoCmd.Roots = []string{testDir}
			stdoutBuffer := &Buffer{
				written: make(chan struct{}, 1),
			}
			wgoCmd.Stdout = stdoutBuffer
			cmdResult := make(chan error, 1)
			go func() {
				cmdResult <- wgoCmd.Run()
			}()

			// Wait for initial script output.
			timer := time.NewTimer(time.Minute)
			defer timer.Stop()
			select {
			case <-stdoutBuffer.written:
			case err := <-cmdResult:
				t.Fatalf("command exited before producing initial output: %v", err)
			case <-timer.C:
				t.Fatal("timed out waiting for initial output")
			}

			// Make changes.
			err = tt.changes(testDir)
			if err != nil {
				t.Fatal(err)
			}

			// Assert the changes were reflected.
			for stdoutBuffer.Len() < len(tt.want) {
				select {
				case <-stdoutBuffer.written:
				case err := <-cmdResult:
					t.Fatalf("command exited before producing expected output: %v", err)
				case <-timer.C:
					t.Fatalf("timed out waiting for output:\n%v", Diff(strings.TrimSpace(stdoutBuffer.String()), tt.want))
				}
			}
			if diff := Diff(strings.TrimSpace(stdoutBuffer.String()), tt.want); diff != "" {
				t.Fatal(diff)
			}

			// Shut down the wgoCmd.
			cancel()
			err = <-cmdResult
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	}
}

func TestStdin(t *testing.T) {
	t.Parallel()

	// Precompile testdata/stdin executable.
	executablePath := filepath.Join(t.TempDir(), "stdin.exe")
	err := exec.Command("go", "build", "-o", executablePath, "./testdata/stdin").Run()
	if err != nil {
		t.Fatal(err)
	}

	// Start the wgoCmd.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wgoCmd, err := WgoCommand(ctx, 0, []string{"-dir", "testdata/stdin", "-stdin", executablePath})
	if err != nil {
		t.Fatal(err)
	}
	pipeReader, pipeWriter := io.Pipe()
	wgoCmd.Stdin = pipeReader
	loggerBuffer := &Buffer{
		written: make(chan struct{}, 1),
	}
	wgoCmd.Logger = log.New(loggerBuffer, "", 0)
	stderrBuffer := &Buffer{
		written: make(chan struct{}, 1),
	}
	wgoCmd.Stderr = stderrBuffer
	cmdResult := make(chan error)
	go func() {
		cmdResult <- wgoCmd.Run()
	}()

	// Wait for script to start (use logger output as the signal).
	timer := time.NewTimer(time.Minute)
	defer timer.Stop()
	for loggerBuffer.Len() == 0 {
		select {
		case <-loggerBuffer.written:
		case err = <-cmdResult:
			t.Fatalf("command exited before starting: %v", err)
		case <-timer.C:
			t.Fatal("timed out waiting for command to start")
		}
	}

	// Write to stdin and close.
	_, err = pipeWriter.Write([]byte("foo\nbar\nbaz\n"))
	if err != nil {
		t.Fatal(err)
	}
	err = pipeWriter.Close()
	if err != nil {
		t.Fatal(err)
	}

	// Assert that what we entered to stdin was echoed to stderr by the script.
	want := `1: foo
2: bar
3: baz`
	for stderrBuffer.Len() < len(want) {
		select {
		case <-stderrBuffer.written:
		case err = <-cmdResult:
			t.Fatalf("command exited before producing expected output: %v", err)
		case <-timer.C:
			t.Fatalf("timed out waiting for output:\n%v", Diff(strings.TrimSpace(stderrBuffer.String()), want))
		}
	}
	if diff := Diff(strings.TrimSpace(stderrBuffer.String()), want); diff != "" {
		t.Fatal(diff)
	}

	// Shut down the wgoCmd.
	cancel()
	err = <-cmdResult
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

// Tests for:
// - https://github.com/bokwoon95/wgo/issues/22
// - https://github.com/bokwoon95/wgo/issues/24
func TestUserInput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creack/pty does not support Windows")
	}
	t.Parallel()

	tempDir := t.TempDir()
	executablePath := filepath.Join(tempDir, "userinput.exe")
	err := exec.Command("go", "build", "-o", executablePath, "./testdata/userinput").Run()
	if err != nil {
		t.Fatal(err)
	}
	pseudoTTY, tty, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer pseudoTTY.Close()
	defer tty.Close()

	watchDir := filepath.Join(tempDir, "watch")
	err = os.Mkdir(watchDir, 0755)
	if err != nil {
		t.Fatal(err)
	}
	watchedPath := filepath.Join(watchDir, "watched.txt")
	err = os.WriteFile(watchedPath, nil, 0666)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wgoCmd, err := WgoCommand(ctx, 0, []string{"-stdin", executablePath})
	if err != nil {
		t.Fatal(err)
	}
	wgoCmd.Roots = []string{watchDir}
	wgoCmd.Stdin = tty
	stdoutBuffer := &Buffer{
		written: make(chan struct{}, 1),
	}
	wgoCmd.Stdout = stdoutBuffer
	cmdResult := make(chan error, 1)
	go func() {
		cmdResult <- wgoCmd.Run()
	}()

	timer := time.NewTimer(time.Minute)
	defer timer.Stop()
	for strings.Count(stdoutBuffer.String(), "User Input: ") < 1 {
		select {
		case <-stdoutBuffer.written:
		case err = <-cmdResult:
			t.Fatalf("command exited before starting: %v", err)
		case <-timer.C:
			t.Fatal("timed out waiting for command to start")
		}
	}
	_, err = pseudoTTY.Write([]byte("123\n"))
	if err != nil {
		t.Fatal(err)
	}
	for !strings.Contains(stdoutBuffer.String(), "Received: 123\nUser Input: ") {
		select {
		case <-stdoutBuffer.written:
		case err = <-cmdResult:
			t.Fatalf("command exited before receiving user input: %v", err)
		case <-timer.C:
			t.Fatalf("timed out waiting for user input:\n%s", stdoutBuffer.String())
		}
	}

	// Leave this input unsubmitted, then trigger a restart. The restart must
	// not wait for a newline, and the terminal must discard the partial line.
	_, err = pseudoTTY.Write([]byte("456"))
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(watchedPath, []byte("restart"), 0666)
	if err != nil {
		t.Fatal(err)
	}
	for strings.Count(stdoutBuffer.String(), "User Input: ") < 3 {
		select {
		case <-stdoutBuffer.written:
		case err = <-cmdResult:
			t.Fatalf("command exited before restarting: %v", err)
		case <-timer.C:
			t.Fatalf("program did not restart until Enter was pressed:\n%s", stdoutBuffer.String())
		}
	}
	_, err = pseudoTTY.Write([]byte("abc\n"))
	if err != nil {
		t.Fatal(err)
	}
	for !strings.Contains(stdoutBuffer.String(), "Received: abc\nUser Input: ") {
		select {
		case <-stdoutBuffer.written:
		case err = <-cmdResult:
			t.Fatalf("command exited before receiving input after restart: %v", err)
		case <-timer.C:
			t.Fatalf("unsubmitted input bled into the restarted program:\n%s", stdoutBuffer.String())
		}
	}
	if strings.Contains(stdoutBuffer.String(), "Received: 456abc") {
		t.Fatalf("unsubmitted input bled into the restarted program:\n%s", stdoutBuffer.String())
	}

	cancel()
	err = <-cmdResult
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestShellWrapping(t *testing.T) {
	t.Parallel()
	// builtins are commands that don't exist in PATH, they are manually
	// handled by the shell. We can use builtin commands to induce an
	// exec.LookPath() error, which will cause WgoCmd to retry by wrapping the
	// command in a shell.
	builtin := ":"
	if runtime.GOOS == "windows" {
		builtin = "Get-Location"
	}

	// Assert that vanilla exec.Command can't find the builtin.
	err := exec.Command(builtin).Run()
	if !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("expected exec.ErrNotFound, got %#v", err)
	}

	// Assert that WgoCommand handles the builtin (via shell wrapping).
	wgoCmd, err := WgoCommand(context.Background(), 0, []string{"-exit", builtin})
	if err != nil {
		t.Fatal(err)
	}
	err = wgoCmd.Run()
	if err != nil {
		t.Error(err)
	}
}

func TestHelp(t *testing.T) {
	ctx := context.WithValue(context.Background(), "flagsetOutput", io.Discard)
	_, err := WgoCommand(ctx, 0, []string{"-h"})
	if !errors.Is(err, flag.ErrHelp) {
		t.Errorf("expected flag.ErrHelp, got %#v", err)
	}
	_, err = WgoCommand(ctx, 0, []string{"run", "-h"})
	if !errors.Is(err, flag.ErrHelp) {
		t.Errorf("expected flag.ErrHelp, got %#v", err)
	}
}

func Diff(got, want any, opts ...cmp.Option) string {
	opts = append(opts,
		cmp.Exporter(func(typ reflect.Type) bool { return true }),
		cmpopts.EquateEmpty(),
	)
	diff := cmp.Diff(got, want, opts...)
	if diff != "" {
		return "\n-got +want\n" + diff
	}
	return ""
}

// Buffer is a custom buffer type that is guarded by a sync.Mutex sends a
// signal down the written channel on Write (non-blocking).
type Buffer struct {
	mutex   sync.Mutex
	buffer  bytes.Buffer
	written chan struct{}
}

func (b *Buffer) Read(p []byte) (n int, err error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.buffer.Read(p)
}

func (b *Buffer) Write(p []byte) (n int, err error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	n, err = b.buffer.Write(p)
	select {
	case b.written <- struct{}{}:
	default:
	}
	return n, err
}

func (b *Buffer) Len() int {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.buffer.Len()
}

func (b *Buffer) String() string {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.buffer.String()
}
