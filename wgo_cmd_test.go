package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
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
	"time"

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
		tt := tt
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
		path        string
		want        bool
	}

	tests := []TestTable{{
		description: "-xfile",
		args:        []string{"-xfile", "_test.go"},
		path:        "wgo_cmd_test.go",
		want:        false,
	}, {
		description: "-xfile with slash",
		args:        []string{"-xfile", "testdata/"},
		path:        "testdata/args/main.go",
		want:        false,
	}, {
		description: "-file",
		args:        []string{"-file", "main.go"},
		path:        "testdata/args/main.go",
		want:        true,
	}, {
		description: "-xdir overrides -file",
		args:        []string{"-file", "main.go", "-xdir", "testdata"},
		path:        "testdata/args/main.go",
		want:        false,
	}, {
		description: "-file matches but -dir does not",
		args:        []string{"-file", "main.go", "-dir", "src"},
		path:        "testdata/args/main.go",
		want:        false,
	}, {
		description: "both -file and -dir match",
		args:        []string{"-file", "main.go", "-dir", "testdata"},
		path:        "testdata/args/main.go",
		want:        true,
	}, {
		description: "-file with slash",
		args:        []string{"-file", "testdata/"},
		path:        "testdata/args/main.go",
		want:        true,
	}, {
		description: "wgo run",
		args:        []string{"run", "."},
		path:        "testdata/args/main.go",
		want:        true,
	}, {
		description: "wgo run without flags exclude non go files",
		args:        []string{"run", "main.go"},
		path:        "testdata/dir/foo/bar.txt",
		want:        false,
	}, {
		description: "fallthrough",
		args:        []string{"-file", ".go", "-file", "test", "-xfile", ".css", "-xfile", "assets"},
		path:        "index.html",
		want:        false,
	}, {
		description: "root is truncated",
		roots:       []string{"/Documents"},
		args:        []string{"-file", "Documents"},
		path:        "/Documents/wgo/main.go",
		want:        false,
	}, {
		description: "root is not truncated",
		roots:       []string{"/lorem_ipsum"},
		args:        []string{"-file", "Documents"},
		path:        "/Documents/wgo/main.go",
		want:        true,
	}, {
		description: "nothing allows anything",
		args:        []string{},
		path:        "/Documents/index.rb",
		want:        true,
	}}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.description, func(t *testing.T) {
			t.Parallel()
			wgoCmd, err := WgoCommand(context.Background(), tt.args)
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
			path, err := filepath.Abs(tt.path)
			if err != nil {
				t.Fatal(err)
			}
			got := wgoCmd.match("", path)
			if !got && tt.want {
				t.Errorf("%v failed to match %q", tt.args, tt.path)
			} else if got && !tt.want {
				t.Errorf("%v incorrectly matches %q", tt.args, tt.path)
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
		tt := tt
		t.Run(tt.description, func(t *testing.T) {
			t.Parallel()
			wgoCmd, err := WgoCommand(context.Background(), tt.args)
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
			wgoCmd.addDirsRecursively(watcher, dir)
			gotWatched := watcher.WatchList()
			sort.Strings(gotWatched)
			sort.Strings(tt.wantWatched)
			if diff := Diff(gotWatched, tt.wantWatched); diff != "" {
				t.Error(diff)
			}
		})
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
			Debounce: 300 * time.Millisecond,
			isRun:    true,
			binPath:  "out",
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
			Debounce: 300 * time.Millisecond,
			isRun:    true,
			binPath:  "out",
		}},
	}, {
		description: "wgo flags",
		args: []string{
			"wgo", "-root", "/secrets", "-file", ".", "-verbose", "echo", "hello",
		},
		wantCmds: []*WgoCmd{{
			Roots:       []string{".", "/secrets"},
			FileRegexps: []*regexp.Regexp{regexp.MustCompile(`.`)},
			ArgsList: [][]string{
				{"echo", "hello"},
			},
			Debounce: 300 * time.Millisecond,
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
		tt := tt
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
			// This is ugly, but because the binPath is randomly generated we
			// have to manually reach into the argslist and overwrite it with a
			// well-known string so that we can compare the commands properly.
			if tt.description == "parallel commands" || tt.description == "build flags" {
				gotCmds[0].binPath = "out"
				gotCmds[0].ArgsList[0][3] = "out"
				gotCmds[0].ArgsList[1][0] = "out"
			}
			opts := []cmp.Option{
				// Comparing loggers always fails, ignore it.
				cmpopts.IgnoreFields(WgoCmd{}, "Logger"),
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
		wgoCmd, err := WgoCommand(context.Background(), []string{
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
		wgoCmd, err := WgoCommand(context.Background(), []string{
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
		wgoCmd, err := WgoCommand(context.Background(), []string{
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
		cmd, err := WgoCommand(context.Background(), []string{
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
		want := "FOO=green\nBAR=lorem ipsum dolor sit amet\nWGO_RANDOM_NUMBER=" + WGO_RANDOM_NUMBER
		if got != want {
			t.Fatalf("\ngot:  %q\nwant: %q", got, want)
		}
	})

	t.Run("timeout off", func(t *testing.T) {
		t.Parallel()
		binPath := "./testdata/hello_world/timeout_off"
		if runtime.GOOS == "windows" {
			binPath += ".exe"
		}
		os.RemoveAll(binPath)
		defer os.RemoveAll(binPath)
		wgoCmd, err := WgoCommand(context.Background(), []string{
			"-exit", "-dir", "testdata/hello_world", "-file", ".go", "go", "build", "-o", binPath, "./testdata/hello_world",
			"::", binPath,
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
		binPath := "./testdata/hello_world/timeout_on"
		if runtime.GOOS == "windows" {
			binPath += ".exe"
		}
		os.RemoveAll(binPath)
		defer os.RemoveAll(binPath)
		wgoCmd, err := WgoCommand(ctx, []string{
			"-exit", "-dir", "testdata/hello_world", "-file", ".go", "go", "build", "-o", binPath, "./testdata/hello_world",
			"::", binPath,
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
		want := ""
		if got != want {
			t.Errorf("\ngot:  %q\nwant: %q", got, want)
		}
	})

	t.Run("signal off", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		wgoCmd, err := WgoCommand(ctx, []string{
			"run", "-dir", "testdata/signal", "./testdata/signal",
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
		want := "Waiting..."
		if got != want {
			t.Errorf("\ngot:  %q\nwant: %q", got, want)
		}
	})

	t.Run("signal on", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Windows doesn't support sending signals to a running process, skipping.")
		}
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		wgoCmd, err := WgoCommand(ctx, []string{
			"run", "-dir", "testdata/signal", "./testdata/signal", "-trap-signal",
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
		want := "Waiting...\nInterrupt received, graceful shutdown."
		if got != want {
			t.Errorf("\ngot:  %q\nwant: %q", got, want)
		}
	})
}

func TestWgoCmd_FileEvent(t *testing.T) {
	t.Parallel()
	os.RemoveAll("testdata/file_event/foo.txt")
	os.RemoveAll("testdata/file_event/internal")
	defer os.RemoveAll("testdata/file_event/foo.txt")
	defer os.RemoveAll("testdata/file_event/internal")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wgoCmd, err := WgoCommand(ctx, []string{"run", "-dir", "testdata/file_event", "-file", ".txt", "./testdata/file_event"})
	if err != nil {
		t.Fatal(err)
	}
	buf := &Buffer{}
	wgoCmd.Stdout = buf
	cmdResult := make(chan error)
	go func() {
		cmdResult <- wgoCmd.Run()
	}()
	time.Sleep(3 * time.Second)

	log.Println("add file")
	err = os.WriteFile("testdata/file_event/foo.txt", []byte("foo"), 0666)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)

	log.Println("edit file")
	err = os.WriteFile("testdata/file_event/foo.txt", []byte("foo fighters"), 0666)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)

	log.Println("create nested directory")
	err = os.MkdirAll("testdata/file_event/internal/baz", 0777)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile("testdata/file_event/foo.txt", []byte("foo"), 0666)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile("testdata/file_event/internal/bar.txt", []byte("bar"), 0666)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile("testdata/file_event/internal/baz/baz.txt", []byte("baz"), 0666)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)

	cancel()
	err = <-cmdResult
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(buf.String())
	want := `---
main.go
run.bat
---
foo.txt: foo
main.go
run.bat
---
foo.txt: foo fighters
main.go
run.bat
---
foo.txt: foo
internal/bar.txt: bar
internal/baz/baz.txt: baz
main.go
run.bat`
	if diff := Diff(got, want); diff != "" {
		t.Error(diff)
	}
}

func TestStdin(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	wgoCmd, err := WgoCommand(ctx, []string{"run", "-exit", "-dir", "testdata/stdin", "-stdin", "./testdata/stdin"})
	if err != nil {
		t.Fatal(err)
	}
	wgoCmd.Stdin = strings.NewReader("foo\nbar\nbaz")
	buf := &Buffer{}
	wgoCmd.Stderr = buf
	err = wgoCmd.Run()
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(buf.String())
	want := "1: foo\n2: bar\n3: baz"
	if got != want {
		t.Errorf("\ngot:  %q\nwant: %q", got, want)
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
	wgoCmd, err := WgoCommand(context.Background(), []string{"-exit", builtin})
	if err != nil {
		t.Fatal(err)
	}
	err = wgoCmd.Run()
	if err != nil {
		t.Error(err)
	}
}

func TestHelp(t *testing.T) {
	_, err := WgoCommand(context.Background(), []string{"-h"})
	if !errors.Is(err, flag.ErrHelp) {
		t.Errorf("expected flag.ErrHelp, got %#v", err)
	}
	_, err = WgoCommand(context.Background(), []string{"run", "-h"})
	if !errors.Is(err, flag.ErrHelp) {
		t.Errorf("expected flag.ErrHelp, got %#v", err)
	}
}

func Diff(got, want interface{}, opts ...cmp.Option) string {
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

// Buffer is a custom buffer type that is guarded by a sync.RWMutex.
//
// Some of the tests (signal on, signal off, timeout on, timeout off) initially
// wrote to a *bytes.Buffer as their Stdout and the *bytes.Buffer was read from
// to assert test results. But these tests occasionally failed with data races
// which caused CI/CD tests to fail and I can't find the cause so I'll just use
// a blunt hammer and use a goroutine-safe buffer for those tests.
type Buffer struct {
	rw  sync.RWMutex
	buf bytes.Buffer
}

func (b *Buffer) Read(p []byte) (n int, err error) {
	b.rw.RLock()
	defer b.rw.RUnlock()
	return b.buf.Read(p)
}

func (b *Buffer) Write(p []byte) (n int, err error) {
	b.rw.Lock()
	defer b.rw.Unlock()
	return b.buf.Write(p)
}

func (b *Buffer) String() string {
	b.rw.Lock()
	defer b.rw.Unlock()
	return b.buf.String()
}
