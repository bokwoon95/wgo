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
		path        string
		isDir       bool
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
	}, {
		description: "directory uses -dir and ignores -file",
		args:        []string{"-dir", "testdata/dir", "-xfile", "dir"},
		path:        "testdata/dir",
		isDir:       true,
		want:        true,
	}, {
		description: "directory excluded by -xdir",
		args:        []string{"-xdir", "testdata/dir"},
		path:        "testdata/dir",
		isDir:       true,
		want:        false,
	}, {
		description: "default excluded directory",
		args:        []string{},
		path:        "testdata/node_modules",
		isDir:       true,
		want:        false,
	}, {
		description: "explicitly included default excluded directory",
		args:        []string{"-dir", "node_modules"},
		path:        "testdata/node_modules",
		isDir:       true,
		want:        true,
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
			path, err := filepath.Abs(tt.path)
			if err != nil {
				t.Fatal(err)
			}
			got := wgoCmd.match("", path, tt.isDir)
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

func TestWgoCmd_pollDirectory(t *testing.T) {
	t.Parallel()

	testDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	wgoCmd := &WgoCmd{
		Roots:        []string{testDir},
		Logger:       defaultLogger,
		PollDuration: 10 * time.Millisecond,
	}
	events := make(chan FileEvent, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		wgoCmd.pollDirectory(ctx, testDir, events)
	}()

	time.Sleep(20 * time.Millisecond)
	filePath := filepath.Join(testDir, "file.txt")
	dirPath := filepath.Join(testDir, "dir")
	if err := os.WriteFile(filePath, []byte("file"), 0666); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dirPath, 0777); err != nil {
		t.Fatal(err)
	}

	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	created := make(map[string]bool)
	for len(created) < 2 {
		select {
		case event := <-events:
			if event.Has(fsnotify.Create) {
				created[event.Name] = event.IsDir
			}
		case <-timer.C:
			t.Fatal("timed out waiting for create events")
		}
	}
	if created[filePath] {
		t.Errorf("file create event marked as directory")
	}
	if !created[dirPath] {
		t.Errorf("directory create event marked as file")
	}

	if err := os.Remove(filePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(dirPath); err != nil {
		t.Fatal(err)
	}
	removed := make(map[string]bool)
	for len(removed) < 2 {
		select {
		case event := <-events:
			if event.Has(fsnotify.Remove) {
				removed[event.Name] = event.IsDir
			}
		case <-timer.C:
			t.Fatal("timed out waiting for remove events")
		}
	}
	if removed[filePath] {
		t.Errorf("file remove event marked as directory")
	}
	if !removed[dirPath] {
		t.Errorf("directory remove event marked as file")
	}

	cancel()
	select {
	case <-done:
	case <-timer.C:
		t.Fatal("timed out waiting for poller to stop")
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
			// This is ugly, but because the binPath is randomly generated we
			// have to manually reach into the argslist and overwrite it with a
			// well-known string so that we can compare the commands properly.
			if tt.description == "parallel commands" || tt.description == "build flags" {
				gotCmds[0].executablePath = "out"
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
		wgoCmd, err := WgoCommand(context.Background(), 0, []string{
			"run", "-exit", "-dir", "testdata/args", "./testdata/args", "apple", "banana", "cherry",
		})
		if err != nil {
			t.Fatal(err)
		}
		buf := &TestBuffer{}
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
		buf := &TestBuffer{}
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
		buf := &TestBuffer{}
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
		buf := &TestBuffer{}
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
		wgoCmd, err := WgoCommand(context.Background(), 0, []string{
			"-exit", "-dir", "testdata/hello_world", "-file", ".go", "go", "build", "-o", binPath, "./testdata/hello_world",
			"::", binPath,
		})
		if err != nil {
			t.Fatal(err)
		}
		buf := &TestBuffer{}
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
		wgoCmd, err := WgoCommand(ctx, 0, []string{
			"-exit", "-dir", "testdata/hello_world", "-file", ".go", "go", "build", "-o", binPath, "./testdata/hello_world",
			"::", binPath,
		})
		if err != nil {
			t.Fatal(err)
		}
		buf := &TestBuffer{}
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
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		wgoCmd, err := WgoCommand(ctx, 0, []string{
			"run", "-dir", "testdata/signal", "./testdata/signal",
		})
		if err != nil {
			t.Fatal(err)
		}
		buf := &TestBuffer{}
		wgoCmd.Stdout = buf
		err = wgoCmd.Run()
		if err != nil {
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
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
		wgoCmd, err := WgoCommand(ctx, 0, []string{
			"run", "-dir", "testdata/signal", "./testdata/signal", "-trap-signal",
		})
		if err != nil {
			t.Fatal(err)
		}
		buf := &TestBuffer{}
		wgoCmd.Stdout = buf
		err = wgoCmd.Run()
		if err != nil {
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
		}
		got := strings.TrimSpace(buf.String())
		want := "Waiting...\nInterrupt received, graceful shutdown."
		if got != want {
			t.Errorf("\ngot:  %q\nwant: %q", got, want)
		}
	})

	t.Run("postpone off", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		wgoCmd, err := WgoCommand(ctx, 0, []string{
			"-xdir", ".", "-xfile", ".", "echo", "hello",
		})
		if err != nil {
			t.Fatal(err)
		}
		buf := &TestBuffer{}
		wgoCmd.Stdout = buf
		err = wgoCmd.Run()
		if err != nil {
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
		}
		got := strings.TrimSpace(buf.String())
		want := "hello"
		if got != want {
			t.Errorf("\ngot:  %q\nwant: %q", got, want)
		}
	})

	t.Run("postpone on", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		wgoCmd, err := WgoCommand(ctx, 0, []string{
			"-xdir", ".", "-xfile", ".", "-postpone", "echo", "hello",
		})
		if err != nil {
			t.Fatal(err)
		}
		buf := &TestBuffer{}
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
}

func TestWgoCmd_FileEvent(t *testing.T) {
	t.Parallel()

	// Cleanup the directory.
	err := os.RemoveAll(filepath.Join("testdata", "file_event", t.Name()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		os.RemoveAll(filepath.Join("testdata", "file_event", t.Name()))
	})

	// Precompile the testdata/file_event executable.
	executablePath := filepath.Join(t.TempDir(), "file_event.exe")
	err = exec.Command("go", "build", "-o", executablePath, "./testdata/file_event").Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		os.Remove(executablePath)
	})

	t.Run("add file", func(t *testing.T) {
		t.Parallel()

		// Initialize the directory.
		testDir := filepath.Join("testdata", "file_event", t.Name())
		initTestDir(t, testDir, fstest.MapFS{
			"alpha.md":      {Data: []byte("alpha")},
			"data/base.bin": {Data: []byte{0x01, 0x02, 0x03}},
		})

		// Start the wgoCmd.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		wgoCmd, err := WgoCommand(ctx, 0, []string{"-dir", filepath.ToSlash(testDir), "-file", ".txt", executablePath, testDir})
		if err != nil {
			t.Fatal(err)
		}
		stdoutBuffer := &TestBuffer{
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
		err = os.WriteFile(filepath.Join(testDir, "added.txt"), []byte("added"), 0666)
		if err != nil {
			t.Fatal(err)
		}

		// Assert the changes were reflected.
		want := `---
alpha.md: alpha
data/base.bin
---
added.txt: added
alpha.md: alpha
data/base.bin`
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

		// Shut down the wgoCmd.
		cancel()
		err = <-cmdResult
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})

	t.Run("edit file", func(t *testing.T) {
		t.Parallel()

		// Initialize the directory.
		testDir := filepath.Join("testdata", "file_event", t.Name())
		initTestDir(t, testDir, fstest.MapFS{
			"config.txt": {Data: []byte("before")},
			"notes.md":   {Data: []byte("notes")},
		})

		// Start the wgoCmd.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		wgoCmd, err := WgoCommand(ctx, 0, []string{"-dir", filepath.ToSlash(testDir), "-file", ".txt", executablePath, testDir})
		if err != nil {
			t.Fatal(err)
		}
		stdoutBuffer := &TestBuffer{
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
		err = os.WriteFile(filepath.Join(testDir, "config.txt"), []byte("after"), 0666)
		if err != nil {
			t.Fatal(err)
		}

		// Assert the changes were reflected.
		want := `---
config.txt: before
notes.md: notes
---
config.txt: after
notes.md: notes`
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

		// Shut down the wgoCmd.
		cancel()
		err = <-cmdResult
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})

	t.Run("create nested directory", func(t *testing.T) {
		t.Parallel()

		// Initialize the directory.
		testDir := filepath.Join("testdata", "file_event", t.Name())
		initTestDir(t, testDir, fstest.MapFS{
			"root.txt":       {Data: []byte("root")},
			"static/keep.md": {Data: []byte("keep")},
		})

		// Start the wgoCmd.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		wgoCmd, err := WgoCommand(ctx, 0, []string{"-dir", filepath.ToSlash(testDir), "-file", ".txt", executablePath, testDir})
		if err != nil {
			t.Fatal(err)
		}
		stdoutBuffer := &TestBuffer{
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
		err = os.MkdirAll(filepath.Join(testDir, "internal", "nested"), 0777)
		if err != nil {
			t.Fatal(err)
		}
		err = os.WriteFile(filepath.Join(testDir, "internal", "bar.txt"), []byte("bar"), 0666)
		if err != nil {
			t.Fatal(err)
		}
		err = os.WriteFile(filepath.Join(testDir, "internal", "nested", "baz.txt"), []byte("baz"), 0666)
		if err != nil {
			t.Fatal(err)
		}
		err = os.WriteFile(filepath.Join(testDir, "root.txt"), []byte("root updated"), 0666)
		if err != nil {
			t.Fatal(err)
		}

		// Assert the changes were reflected.
		want := `---
root.txt: root
static/keep.md: keep
---
internal/bar.txt: bar
internal/nested/baz.txt: baz
root.txt: root updated
static/keep.md: keep`
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

		// Shut down the wgoCmd.
		cancel()
		err = <-cmdResult
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})

	t.Run("remove file", func(t *testing.T) {
		t.Parallel()

		testDir := filepath.Join("testdata", "file_event", t.Name())
		initTestDir(t, testDir, fstest.MapFS{
			"keep.md":    {Data: []byte("keep")},
			"target.txt": {Data: []byte("target")},
		})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		wgoCmd, err := WgoCommand(ctx, 0, []string{"-dir", filepath.ToSlash(testDir), "-file", ".txt", executablePath, testDir})
		if err != nil {
			t.Fatal(err)
		}
		stdoutBuffer := &TestBuffer{
			written: make(chan struct{}, 1),
		}
		wgoCmd.Stdout = stdoutBuffer
		cmdResult := make(chan error, 1)
		go func() {
			cmdResult <- wgoCmd.Run()
		}()

		timer := time.NewTimer(time.Minute)
		defer timer.Stop()
		select {
		case <-stdoutBuffer.written:
		case err := <-cmdResult:
			t.Fatalf("command exited before producing initial output: %v", err)
		case <-timer.C:
			t.Fatal("timed out waiting for initial output")
		}

		err = os.Remove(filepath.Join(testDir, "target.txt"))
		if err != nil {
			t.Fatal(err)
		}

		want := "---\nkeep.md: keep\ntarget.txt: target\n---\nkeep.md: keep"
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

	t.Run("rename file", func(t *testing.T) {
		t.Parallel()

		testDir := filepath.Join("testdata", "file_event", t.Name())
		initTestDir(t, testDir, fstest.MapFS{
			"keep.md":    {Data: []byte("keep")},
			"target.txt": {Data: []byte("target")},
		})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		wgoCmd, err := WgoCommand(ctx, 0, []string{"-dir", filepath.ToSlash(testDir), "-file", ".txt", executablePath, testDir})
		if err != nil {
			t.Fatal(err)
		}
		stdoutBuffer := &TestBuffer{
			written: make(chan struct{}, 1),
		}
		wgoCmd.Stdout = stdoutBuffer
		cmdResult := make(chan error, 1)
		go func() {
			cmdResult <- wgoCmd.Run()
		}()

		timer := time.NewTimer(time.Minute)
		defer timer.Stop()
		select {
		case <-stdoutBuffer.written:
		case err := <-cmdResult:
			t.Fatalf("command exited before producing initial output: %v", err)
		case <-timer.C:
			t.Fatal("timed out waiting for initial output")
		}

		err = os.Rename(filepath.Join(testDir, "target.txt"), filepath.Join(testDir, "target.md"))
		if err != nil {
			t.Fatal(err)
		}

		want := "---\nkeep.md: keep\ntarget.txt: target\n---\nkeep.md: keep\ntarget.md: target"
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

func TestWgoCmd_Polling(t *testing.T) {
	t.Parallel()

	// Cleanup the directory.
	err := os.RemoveAll(filepath.Join("testdata", "polling", t.Name()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		os.RemoveAll(filepath.Join("testdata", "polling", t.Name()))
	})

	// Precompile the testdata/polling executable.
	executablePath := filepath.Join(t.TempDir(), "polling.exe")
	err = exec.Command("go", "build", "-o", executablePath, "./testdata/polling").Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		os.Remove(executablePath)
	})

	t.Run("add file", func(t *testing.T) {
		t.Parallel()

		// Initialize the directory.
		testDir := filepath.Join("testdata", "polling", t.Name())
		initTestDir(t, testDir, fstest.MapFS{
			"alpha.md":      {Data: []byte("alpha")},
			"data/base.bin": {Data: []byte{0x01, 0x02, 0x03}},
		})

		// Start the wgoCmd.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		wgoCmd, err := WgoCommand(ctx, 0, []string{"-dir", filepath.ToSlash(testDir), "-file", ".txt", "-poll", "100ms", executablePath, testDir})
		if err != nil {
			t.Fatal(err)
		}
		stdoutBuffer := &TestBuffer{
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
		err = os.WriteFile(filepath.Join(testDir, "added.txt"), []byte("added"), 0666)
		if err != nil {
			t.Fatal(err)
		}

		// Assert the changes were reflected.
		want := `---
alpha.md: alpha
data/base.bin
---
added.txt: added
alpha.md: alpha
data/base.bin`
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

		// Shut down the wgoCmd.
		cancel()
		err = <-cmdResult
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})

	t.Run("edit file", func(t *testing.T) {
		t.Parallel()

		// Initialize the directory.
		testDir := filepath.Join("testdata", "polling", t.Name())
		initTestDir(t, testDir, fstest.MapFS{
			"config.txt": {Data: []byte("before")},
			"notes.md":   {Data: []byte("notes")},
		})

		// Start the wgoCmd.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		wgoCmd, err := WgoCommand(ctx, 0, []string{"-dir", filepath.ToSlash(testDir), "-file", ".txt", "-poll", "100ms", executablePath, testDir})
		if err != nil {
			t.Fatal(err)
		}
		stdoutBuffer := &TestBuffer{
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
		err = os.WriteFile(filepath.Join(testDir, "config.txt"), []byte("after"), 0666)
		if err != nil {
			t.Fatal(err)
		}

		// Assert the changes were reflected.
		want := `---
config.txt: before
notes.md: notes
---
config.txt: after
notes.md: notes`
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

		// Shut down the wgoCmd.
		cancel()
		err = <-cmdResult
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})

	t.Run("create nested directory", func(t *testing.T) {
		t.Parallel()

		// Initialize the directory.
		testDir := filepath.Join("testdata", "polling", t.Name())
		initTestDir(t, testDir, fstest.MapFS{
			"root.txt":       {Data: []byte("root")},
			"static/keep.md": {Data: []byte("keep")},
		})

		// Start the wgoCmd.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		wgoCmd, err := WgoCommand(ctx, 0, []string{"-dir", filepath.ToSlash(testDir), "-file", ".txt", "-poll", "100ms", executablePath, testDir})
		if err != nil {
			t.Fatal(err)
		}
		stdoutBuffer := &TestBuffer{
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
		err = os.MkdirAll(filepath.Join(testDir, "internal", "nested"), 0777)
		if err != nil {
			t.Fatal(err)
		}
		err = os.WriteFile(filepath.Join(testDir, "internal", "bar.txt"), []byte("bar"), 0666)
		if err != nil {
			t.Fatal(err)
		}
		err = os.WriteFile(filepath.Join(testDir, "internal", "nested", "baz.txt"), []byte("baz"), 0666)
		if err != nil {
			t.Fatal(err)
		}
		err = os.WriteFile(filepath.Join(testDir, "root.txt"), []byte("root updated"), 0666)
		if err != nil {
			t.Fatal(err)
		}

		// Assert the changes were reflected.
		want := `---
root.txt: root
static/keep.md: keep
---
internal/bar.txt: bar
internal/nested/baz.txt: baz
root.txt: root updated
static/keep.md: keep`
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

		// Shut down the wgoCmd.
		cancel()
		err = <-cmdResult
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})

	t.Run("remove file", func(t *testing.T) {
		t.Parallel()

		testDir := filepath.Join("testdata", "polling", t.Name())
		initTestDir(t, testDir, fstest.MapFS{
			"keep.md":    {Data: []byte("keep")},
			"target.txt": {Data: []byte("target")},
		})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		wgoCmd, err := WgoCommand(ctx, 0, []string{"-dir", filepath.ToSlash(testDir), "-file", ".txt", "-poll", "100ms", executablePath, testDir})
		if err != nil {
			t.Fatal(err)
		}
		stdoutBuffer := &TestBuffer{
			written: make(chan struct{}, 1),
		}
		wgoCmd.Stdout = stdoutBuffer
		cmdResult := make(chan error, 1)
		go func() {
			cmdResult <- wgoCmd.Run()
		}()

		timer := time.NewTimer(time.Minute)
		defer timer.Stop()
		select {
		case <-stdoutBuffer.written:
		case err := <-cmdResult:
			t.Fatalf("command exited before producing initial output: %v", err)
		case <-timer.C:
			t.Fatal("timed out waiting for initial output")
		}

		err = os.Remove(filepath.Join(testDir, "target.txt"))
		if err != nil {
			t.Fatal(err)
		}

		want := "---\nkeep.md: keep\ntarget.txt: target\n---\nkeep.md: keep"
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

	t.Run("rename file", func(t *testing.T) {
		t.Parallel()

		testDir := filepath.Join("testdata", "polling", t.Name())
		initTestDir(t, testDir, fstest.MapFS{
			"keep.md":    {Data: []byte("keep")},
			"target.txt": {Data: []byte("target")},
		})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		wgoCmd, err := WgoCommand(ctx, 0, []string{"-dir", filepath.ToSlash(testDir), "-file", ".txt", "-poll", "100ms", executablePath, testDir})
		if err != nil {
			t.Fatal(err)
		}
		stdoutBuffer := &TestBuffer{
			written: make(chan struct{}, 1),
		}
		wgoCmd.Stdout = stdoutBuffer
		cmdResult := make(chan error, 1)
		go func() {
			cmdResult <- wgoCmd.Run()
		}()

		timer := time.NewTimer(time.Minute)
		defer timer.Stop()
		select {
		case <-stdoutBuffer.written:
		case err := <-cmdResult:
			t.Fatalf("command exited before producing initial output: %v", err)
		case <-timer.C:
			t.Fatal("timed out waiting for initial output")
		}

		err = os.Rename(filepath.Join(testDir, "target.txt"), filepath.Join(testDir, "target.md"))
		if err != nil {
			t.Fatal(err)
		}

		want := "---\nkeep.md: keep\ntarget.txt: target\n---\nkeep.md: keep\ntarget.md: target"
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

func TestStdin(t *testing.T) {
	t.Parallel()

	// Precompile testdata/stdin executable.
	executablePath := filepath.Join(os.TempDir(), t.Name()+".exe")
	err := exec.Command("go", "build", "-o", executablePath, "./testdata/stdin").Run()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(executablePath)

	// Start the wgoCmd.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wgoCmd, err := WgoCommand(ctx, 0, []string{"-dir", "testdata/stdin", "-stdin", executablePath})
	if err != nil {
		t.Fatal(err)
	}
	pipeReader, pipeWriter := io.Pipe()
	wgoCmd.Stdin = pipeReader
	loggerBuffer := &TestBuffer{
		written: make(chan struct{}, 1),
	}
	wgoCmd.Logger = log.New(loggerBuffer, "", 0)
	stderrBuffer := &TestBuffer{
		written: make(chan struct{}, 1),
	}
	wgoCmd.Stderr = stderrBuffer
	cmdResult := make(chan error)
	go func() {
		cmdResult <- wgoCmd.Run()
	}()

	// Wait for the script to start. wgoCmd first prints EXECUTING ... to the
	// logger, use that as the signal.
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
	want := "1: foo\n2: bar\n3: baz"
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
	_, err := WgoCommand(context.Background(), 0, []string{"-h"})
	if !errors.Is(err, flag.ErrHelp) {
		t.Errorf("expected flag.ErrHelp, got %#v", err)
	}
	_, err = WgoCommand(context.Background(), 0, []string{"run", "-h"})
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

// initTestDir initializes the testDir to mirror the contents of mapFS.
func initTestDir(t *testing.T, testDir string, mapFS fstest.MapFS) {
	t.Helper()

	// Cleanup the directory.
	err := os.RemoveAll(testDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		os.RemoveAll(testDir)
	})

	// Create the directory.
	err = os.MkdirAll(testDir, 0777)
	if err != nil {
		t.Fatal(err)
	}

	// Create the directory contents.
	err = fs.WalkDir(mapFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == "." {
			return nil
		}
		destPath := filepath.Join(testDir, filepath.FromSlash(path))
		if d.IsDir() {
			return os.MkdirAll(destPath, 0777)
		}
		contents, err := fs.ReadFile(mapFS, path)
		if err != nil {
			return err
		}
		return os.WriteFile(destPath, contents, 0666)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestBuffer is a custom buffer type that is guarded by a sync.Mutex and on
// write it attempts to send a signal (best-effort) down the written channel.
type TestBuffer struct {
	mutex   sync.Mutex
	buffer  bytes.Buffer
	written chan struct{}
}

func (b *TestBuffer) Read(p []byte) (n int, err error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.buffer.Read(p)
}

func (b *TestBuffer) Write(p []byte) (n int, err error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	n, err = b.buffer.Write(p)
	select {
	case b.written <- struct{}{}:
	default:
	}
	return n, err
}

func (b *TestBuffer) Len() int {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.buffer.Len()
}

func (b *TestBuffer) String() string {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.buffer.String()
}
