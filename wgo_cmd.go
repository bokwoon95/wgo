package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/fsnotify/fsnotify"
)

// String flag names copied from `go help build`.
var strFlagNames = []string{
	"p", "asmflags", "buildmode", "compiler", "gccgoflags", "gcflags",
	"installsuffix", "ldflags", "mod", "modfile", "overlay", "pkgdir",
	"tags", "toolexec", "exec",
}

// Bool flag names copied from `go help build`.
var boolFlagNames = []string{
	"a", "n", "race", "msan", "asan", "v", "work", "x", "buildvcs",
	"linkshared", "modcacherw", "trimpath",
}

var defaultLogger = log.New(io.Discard, "", 0)

func init() {
	rand.Seed(time.Now().Unix())
}

// WgoCmd implements the `wgo` command.
type WgoCmd struct {
	// The root directories to watch for changes in. Earlier roots have higher
	// precedence than later roots (used during file matching). Roots must
	// always use forward slash file separators, even on Windows.
	Roots []string

	// FileRegexps specifies the file patterns to include. They are matched
	// against the a file's path relative to the root. File patterns are
	// logically OR-ed together, so you can include multiple patterns at once.
	// All patterns must use forward slash file separators, even on Windows.
	//
	// If no FileRegexps are provided, every file is included by default unless
	// it is explicitly excluded by ExcludeFileRegexps.
	FileRegexps []*regexp.Regexp

	// ExcludeFileRegexps specifies the file patterns to exclude. They are
	// matched against a file's path relative to the root. File patterns are
	// logically OR-ed together, so you can exclude multiple patterns at once.
	// All patterns must use forward slash separators, even on Windows.
	//
	// Excluded file patterns take higher precedence than included file
	// patterns, so you can include a large group of files using an include
	// pattern and surgically ignore specific files from that group using an
	// exclude pattern.
	ExcludeFileRegexps []*regexp.Regexp

	// DirRegexps specifies the directory patterns to include. They are matched
	// against a directory's path relative to the root. Directory patterns are
	// logically OR-ed together, so you can include multiple patterns at once.
	// All patterns must use forward slash separators, even on Windows.
	//
	// If no DirRegexps are provided, every directory is included by default
	// unless it is explicitly excluded by ExcludeDirRegexps.
	DirRegexps []*regexp.Regexp

	// ExcludeDirRegexps specifies the directory patterns to exclude. They are
	// matched against a directory's path relative to the root. Directory
	// patterns are logically OR-ed together, so you can exclude multiple
	// patterns at once. All patterns must use forward slash separators, even
	// on Windows.
	ExcludeDirRegexps []*regexp.Regexp

	// If provided, Logger is used to log file events.
	Logger *log.Logger

	// ArgsList is the list of args slices. Each slice corresponds to a single
	// command to execute and is of this form [cmd arg1 arg2 arg3...]. A slice
	// of these commands represent the chain of commands to be executed.
	ArgsList [][]string

	// Env is sets the environment variables for the commands. Each entry is of
	// the form "KEY=VALUE".
	Env []string

	// Dir specifies the working directory for the commands.
	Dir string

	// EnableStdin controls whether the Stdin field is used.
	EnableStdin bool

	// Stdin is where the last command gets its stdin input from (EnableStdin
	// must be true).
	Stdin io.Reader

	// Stdout is where the commands write their stdout output.
	Stdout io.Writer

	// Stderr is where the commands write their stderr output.
	Stderr io.Writer

	// If Exit is true, WgoCmd exits once the last command exits.
	Exit bool

	// DebounceDuration is the debounce duration for file events.
	DebounceDuration time.Duration

	// PollDuration is the duration at which we poll for events. The zero value
	// means no polling.
	PollDuration time.Duration

	ctx     context.Context
	isRun   bool   // Whether the command is `wgo run`.
	binPath string // Where the built go binary lives.
}

// WgoCommands instantiates a slices of WgoCmds. Each "::" separator followed
// by "wgo" indicates a new WgoCmd.
func WgoCommands(ctx context.Context, args []string) ([]*WgoCmd, error) {
	var wgoCmds []*WgoCmd
	i, j, num := 1, 1, 1
	for j < len(args) {
		if args[j] != "::" || j+1 >= len(args) || args[j+1] != "wgo" {
			j++
			continue
		}
		wgoCmd, err := WgoCommand(ctx, args[i:j])
		if err != nil {
			return nil, fmt.Errorf("[wgo %d] %w", num, err)
		}
		wgoCmds = append(wgoCmds, wgoCmd)
		i, j, num = j+2, j+2, num+1
	}
	if j > i {
		wgoCmd, err := WgoCommand(ctx, args[i:j])
		if err != nil {
			return nil, fmt.Errorf("[wgo %d] %w", num, err)
		}
		wgoCmds = append(wgoCmds, wgoCmd)
	}
	return wgoCmds, nil
}

// WgoCommand instantiates a new WgoCmd. Each "::" separator indicates a new
// chained command.
func WgoCommand(ctx context.Context, args []string) (*WgoCmd, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	wgoCmd := WgoCmd{
		Roots:  []string{filepath.ToSlash(cwd)},
		Logger: defaultLogger,
		ctx:    ctx,
	}
	var verbose bool
	wgoCmd.isRun = len(args) > 0 && args[0] == "run"
	if wgoCmd.isRun {
		args = args[1:]
	}

	// Parse flags.
	var debounce, poll string
	flagset := flag.NewFlagSet("", flag.ContinueOnError)
	flagset.StringVar(&wgoCmd.Dir, "cd", "", "Change to a different directory to run the commands.")
	flagset.BoolVar(&verbose, "verbose", false, "Log file events.")
	flagset.BoolVar(&wgoCmd.Exit, "exit", false, "Exit when the last command exits.")
	flagset.BoolVar(&wgoCmd.EnableStdin, "stdin", false, "Enable stdin for the last command.")
	flagset.StringVar(&debounce, "debounce", "300ms", "How quickly to react to file events. Lower debounce values will react quicker.")
	flagset.StringVar(&poll, "poll", "", "How often to poll for file changes e.g. 1s. No value means no polling.")
	flagset.Func("root", "Specify an additional root directory to watch. Can be repeated.", func(value string) error {
		root, err := filepath.Abs(value)
		if err != nil {
			return err
		}
		wgoCmd.Roots = append(wgoCmd.Roots, filepath.ToSlash(root))
		return nil
	})
	flagset.Func("file", "Include file regex. Can be repeated.", func(value string) error {
		r, err := compileRegexp(value)
		if err != nil {
			return err
		}
		wgoCmd.FileRegexps = append(wgoCmd.FileRegexps, r)
		return nil
	})
	flagset.Func("xfile", "Exclude file regex. Can be repeated.", func(value string) error {
		r, err := compileRegexp(value)
		if err != nil {
			return err
		}
		wgoCmd.ExcludeFileRegexps = append(wgoCmd.ExcludeFileRegexps, r)
		return nil
	})
	flagset.Func("dir", "Include directory regex. Can be repeated.", func(value string) error {
		r, err := compileRegexp(value)
		if err != nil {
			return err
		}
		wgoCmd.DirRegexps = append(wgoCmd.DirRegexps, r)
		return nil
	})
	flagset.Func("xdir", "Exclude directory regex. Can be repeated.", func(value string) error {
		r, err := compileRegexp(value)
		if err != nil {
			return err
		}
		wgoCmd.ExcludeDirRegexps = append(wgoCmd.ExcludeDirRegexps, r)
		return nil
	})
	flagset.Usage = func() {
		fmt.Fprint(flagset.Output(), `Usage:
  wgo [FLAGS] <command> [ARGUMENTS...]
  wgo gcc -o main main.c
  wgo go build -o main main.go
  wgo -file .c gcc -o main main.c
  wgo -file=.go go build -o main main.go
Flags:
`)
		flagset.PrintDefaults()
	}
	// If the command is `wgo run`, also parse the go build flags.
	var strFlagValues []string
	var boolFlagValues []bool
	if wgoCmd.isRun {
		strFlagValues = make([]string, 0, len(strFlagNames))
		for i := range strFlagNames {
			name := strFlagNames[i]
			flagset.Func(name, "-"+name+" build flag for Go.", func(value string) error {
				strFlagValues = append(strFlagValues, "-"+name, value)
				return nil
			})
		}
		boolFlagValues = make([]bool, len(boolFlagNames))
		for i := range boolFlagNames {
			name := boolFlagNames[i]
			flagset.BoolVar(&boolFlagValues[i], name, false, "-"+name+" build flag for Go.")
		}
		flagset.Usage = func() {
			fmt.Fprint(flagset.Output(), `Usage:
  wgo run [FLAGS] [GO_BUILD_FLAGS] <package> [ARGUMENTS...]
  wgo run main.go
  wgo run -file .html main.go arg1 arg2 arg3
  wgo run -file .html . arg1 arg2 arg3
  wgo run -file=.css -file=.js -tags=fts5 ./cmd/my_project arg1 arg2 arg3
Flags:
`)
			flagset.PrintDefaults()
		}
	}
	err = flagset.Parse(args)
	if err != nil {
		return nil, err
	}
	if verbose {
		wgoCmd.Logger = log.New(os.Stderr, "[wgo] ", 0)
	}
	if debounce != "" {
		wgoCmd.DebounceDuration, err = time.ParseDuration(debounce)
		if err != nil {
			return nil, fmt.Errorf("-debounce: %w", err)
		}
	} else {
		wgoCmd.DebounceDuration = 300 * time.Millisecond
	}
	if poll != "" {
		wgoCmd.PollDuration, err = time.ParseDuration(poll)
		if err != nil {
			return nil, fmt.Errorf("-poll: %w", err)
		}
	}

	// If the command is `wgo run`, prepend a `go build` command to the
	// ArgsList.
	flagArgs := flagset.Args()
	wgoCmd.ArgsList = append(wgoCmd.ArgsList, []string{})
	if wgoCmd.isRun {
		if len(flagArgs) == 0 {
			return nil, fmt.Errorf("wgo run: package not provided")
		}
		// Determine the temp directory to put the binary in.
		// https://github.com/golang/go/issues/8451#issuecomment-341475329
		tmpDir := os.Getenv("GOTMPDIR")
		if tmpDir == "" {
			tmpDir = os.TempDir()
		}
		pkg := flagArgs[0]
		if pkg == "." {
			pkg = filepath.Base(cwd)
		}
		wgoCmd.binPath = filepath.Join(tmpDir, "wgo-"+time.Now().Format("2006-01-02-150405-999"), pkg)
		if runtime.GOOS == "windows" {
			wgoCmd.binPath += ".exe"
		}
		buildArgs := []string{"go", "build", "-o", wgoCmd.binPath}
		buildArgs = append(buildArgs, strFlagValues...)
		for i, ok := range boolFlagValues {
			if ok {
				buildArgs = append(buildArgs, "-"+boolFlagNames[i])
			}
		}
		buildArgs = append(buildArgs, flagArgs[0])
		runArgs := []string{wgoCmd.binPath}
		wgoCmd.ArgsList = [][]string{buildArgs, runArgs}
		flagArgs = flagArgs[1:]
	}

	for _, arg := range flagArgs {
		// If arg is "::", start a new command.
		if arg == "::" {
			wgoCmd.ArgsList = append(wgoCmd.ArgsList, []string{})
			continue
		}

		// Unescape ":::" => "::", "::::" => ":::", etc.
		allColons := len(arg) > 2
		for _, c := range arg {
			if c != ':' {
				allColons = false
				break
			}
		}
		if allColons {
			arg = arg[1:]
		}

		// Append arg to the last command in the chain.
		n := len(wgoCmd.ArgsList) - 1
		wgoCmd.ArgsList[n] = append(wgoCmd.ArgsList[n], arg)
	}
	return &wgoCmd, nil
}

// Run runs the WgoCmd.
func (wgoCmd *WgoCmd) Run() error {
	if wgoCmd.Stdin == nil {
		wgoCmd.Stdin = os.Stdin
	}
	if wgoCmd.Stdout == nil {
		wgoCmd.Stdout = os.Stdout
	}
	if wgoCmd.Stderr == nil {
		wgoCmd.Stderr = os.Stderr
	}
	if wgoCmd.Logger == nil {
		wgoCmd.Logger = defaultLogger
	}
	for i, root := range wgoCmd.Roots {
		root, err := filepath.Abs(root)
		if err != nil {
			return err
		}
		wgoCmd.Roots[i] = filepath.ToSlash(root)
	}
	if wgoCmd.binPath != "" {
		defer os.Remove(wgoCmd.binPath)
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()
	for _, root := range wgoCmd.Roots {
		if wgoCmd.PollDuration == 0 {
			wgoCmd.addDirsRecursively(watcher, root)
		} else {
			wgoCmd.pollDirectory(wgoCmd.ctx, root, watcher.Events)
		}
	}
	// Timer is used to debounce events. Each event does not directly trigger a
	// reload, it only resets the timer. Only when the timer is allowed to
	// fully expire will the reload actually occur.
	timer := time.NewTimer(0)
	timer.Stop()

	for {
	CMD_CHAIN:
		for i, args := range wgoCmd.ArgsList {
			// Step 1: Prepare the command.
			//
			// We are not using exec.CommandContext() because it uses
			// cmd.Process.Kill() to kill the process, but we want to use our
			// custom stop() function to kill the process. Our stop() function
			// is better than cmd.Process.Kill() because it kills the child
			// processes as well.
			cmd := &exec.Cmd{
				Path:   args[0],
				Args:   args,
				Env:    wgoCmd.Env,
				Dir:    wgoCmd.Dir,
				Stdout: wgoCmd.Stdout,
				Stderr: wgoCmd.Stderr,
			}
			setpgid(cmd)
			if filepath.Base(cmd.Path) == cmd.Path {
				cmd.Path, err = exec.LookPath(cmd.Path)
				if errors.Is(err, exec.ErrNotFound) {
					if runtime.GOOS == "windows" {
						path, err := exec.LookPath("pwsh.exe")
						if err != nil {
							return err
						}
						cmd.Path = path
						cmd.Args = []string{"pwsh.exe", "-command", joinArgs(args)}
					} else {
						path, err := exec.LookPath("sh")
						if err != nil {
							return err
						}
						cmd.Path = path
						cmd.Args = []string{"sh", "-c", joinArgs(args)}
					}
				} else if err != nil {
					return err
				}
			}
			// If the user enabled it, feed wgoCmd.Stdin to the command's
			// Stdin. Only the last command gets to read from Stdin -- if we
			// give Stdin to every command in the middle it will prevent the
			// next command from being executed if they don't consume Stdin.
			//
			// We have to use cmd.StdinPipe() here instead of assigning
			// cmd.Stdin directly, otherwise `wgo run ./testdata/stdin` doesn't
			// work interactively (the tests will pass, but somehow it won't
			// actually work if you run it in person. I don't know why).
			var wg sync.WaitGroup
			if wgoCmd.EnableStdin && i == len(wgoCmd.ArgsList)-1 {
				stdinPipe, err := cmd.StdinPipe()
				if err != nil {
					return err
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer stdinPipe.Close()
					_, _ = io.Copy(stdinPipe, wgoCmd.Stdin)
				}()
			}

			// Step 2: Run the command in the background.
			cmdResult := make(chan error, 1)
			waitDone := make(chan struct{})
			err = cmd.Start()
			if err != nil {
				return err
			}
			go func() {
				wg.Wait()
				cmdResult <- cmd.Wait()
				close(waitDone)
			}()

			// Step 3: Wait for events in the event loop.
			for {
				select {
				case <-wgoCmd.ctx.Done():
					stop(cmd)
					<-waitDone
					return nil
				case err := <-cmdResult:
					if i == len(wgoCmd.ArgsList)-1 {
						if wgoCmd.Exit {
							return err
						}
						break
					}
					if err != nil {
						break
					}
					continue CMD_CHAIN
				case err := <-watcher.Errors:
					wgoCmd.Logger.Println(err)
				case event := <-watcher.Events:
					// We don't handle Remove events because once an item has
					// been deleted, we cannot determine if it is a file or
					// directory. Therefore, we cannot determine which regex
					// matching rules should apply (-file/-xfile for files,
					// -dir/-xdir for directories). So, we ignore deleted files.
					if !event.Has(fsnotify.Create) && !event.Has(fsnotify.Write) {
						continue
					}
					fileInfo, err := os.Stat(event.Name)
					if err != nil {
						continue
					}
					name := filepath.ToSlash(event.Name)
					if fileInfo.IsDir() {
						if wgoCmd.PollDuration == 0 && event.Has(fsnotify.Create) {
							wgoCmd.addDirsRecursively(watcher, event.Name)
						}
						if wgoCmd.matchDir(name) {
							wgoCmd.Logger.Println(event.Op.String(), name)
							timer.Reset(wgoCmd.DebounceDuration)
						} else {
							wgoCmd.Logger.Println("(skip)", event.Op.String(), name)
						}
					} else {
						if wgoCmd.matchFile(event.Name) {
							wgoCmd.Logger.Println(event.Op.String(), name)
							timer.Reset(wgoCmd.DebounceDuration)
						} else {
							wgoCmd.Logger.Println("(skip)", event.Op.String(), name)
						}
					}
				case <-timer.C: // Timer expired, reload commands.
					stop(cmd)
					<-waitDone
					break CMD_CHAIN
				}
			}
		}
	}
}

// compileRegexp is like regexp.Compile except it treats dots followed by
// [a-zA-Z] as a dot literal. Makes expressing file extensions like .css or
// .html easier. The user can always escape this behaviour by wrapping the dot
// up in a grouping bracket i.e. `(.)css`.
func compileRegexp(pattern string) (*regexp.Regexp, error) {
	n := strings.Count(pattern, ".")
	if n == 0 {
		return regexp.Compile(pattern)
	}
	if strings.HasPrefix(pattern, "./") && len(pattern) > 2 {
		// Any pattern starting with "./" is almost certainly a mistake - it
		// looks like it refers to the current directory when in actuality any
		// regex starting with "./" matches nothing in the current directory
		// because of the slash in front. Nobody every really means to match
		// "one character followed by a slash" so we accomodate this common use
		// case and trim the "./" prefix away.
		pattern = pattern[2:]
	}
	var b strings.Builder
	b.Grow(len(pattern) + n)
	j := 0
	for j < len(pattern) {
		prev, _ := utf8.DecodeLastRuneInString(b.String())
		curr, width := utf8.DecodeRuneInString(pattern[j:])
		next, _ := utf8.DecodeRuneInString(pattern[j+width:])
		j += width
		if prev != '\\' && curr == '.' && (('a' <= next && next <= 'z') || ('A' <= next && next <= 'Z')) {
			b.WriteString("\\.")
		} else {
			b.WriteRune(curr)
		}
	}
	return regexp.Compile(b.String())
}

// addDirsRecursively adds directories recursively to a watcher since it
// doesn't support it natively https://github.com/fsnotify/fsnotify/issues/18.
// A nice side effect is that we get to log the watched directories as we go.
func (wgoCmd *WgoCmd) addDirsRecursively(watcher *fsnotify.Watcher, dir string) {
	roots := make(map[string]struct{})
	for _, root := range wgoCmd.Roots {
		roots[root] = struct{}{}
	}
	_ = filepath.WalkDir(dir, func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		name = filepath.ToSlash(name)
		_, isRoot := roots[name]
		if isRoot {
			wgoCmd.Logger.Println("WATCH", name)
			watcher.Add(name)
			return nil
		}
		for _, root := range wgoCmd.Roots {
			if strings.HasPrefix(name, root+"/") {
				name = strings.TrimPrefix(name, root+"/")
				break
			}
		}
		for _, r := range wgoCmd.ExcludeDirRegexps {
			if r.MatchString(name) {
				return filepath.SkipDir
			}
		}
		for _, r := range wgoCmd.DirRegexps {
			if r.MatchString(name) {
				wgoCmd.Logger.Println("WATCH", name)
				watcher.Add(name)
				return nil
			}
		}
		switch filepath.Base(name) {
		case ".git", ".hg", ".svn", ".idea", ".vscode", ".settings", "node_modules":
			return filepath.SkipDir
		}
		if strings.HasPrefix(filepath.Base(name), ".") {
			return filepath.SkipDir
		}
		wgoCmd.Logger.Println("WATCH", name)
		watcher.Add(name)
		return nil
	})
}

// matchFile checks if a given dir path should trigger a reload.
func (wgoCmd *WgoCmd) matchDir(name string) bool {
	for _, root := range wgoCmd.Roots {
		if strings.HasPrefix(name, root+"/") {
			name = strings.TrimPrefix(name, root+"/")
			break
		}
	}
	for _, r := range wgoCmd.ExcludeDirRegexps {
		if r.MatchString(name) {
			return false
		}
	}
	for _, r := range wgoCmd.DirRegexps {
		if r.MatchString(name) {
			return true
		}
	}
	if len(wgoCmd.DirRegexps) == 0 {
		return true
	}
	return false
}

// matchFile checks if a given file path should trigger a reload.
func (wgoCmd *WgoCmd) matchFile(name string) bool {
	dir := filepath.ToSlash(filepath.Dir(name))
	for _, root := range wgoCmd.Roots {
		if strings.HasPrefix(name, root+"/") {
			name = strings.TrimPrefix(name, root+"/")
			dir = filepath.ToSlash(filepath.Dir(name))
			break
		}
	}
	for _, r := range wgoCmd.ExcludeDirRegexps {
		if r.MatchString(dir) {
			return false
		}
	}
	if len(wgoCmd.DirRegexps) > 0 {
		matched := false
		for _, r := range wgoCmd.DirRegexps {
			if r.MatchString(dir) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	for _, r := range wgoCmd.ExcludeFileRegexps {
		if r.MatchString(name) {
			return false
		}
	}
	for _, r := range wgoCmd.FileRegexps {
		if r.MatchString(name) {
			return true
		}
	}
	if wgoCmd.isRun {
		if strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
			return true
		}
		return false
	}
	if len(wgoCmd.FileRegexps) == 0 {
		return true
	}
	return false
}

func (wgoCmd *WgoCmd) pollDirectory(ctx context.Context, name string, events chan<- fsnotify.Event) {
	// wg tracks the number of active goroutines.
	var wg sync.WaitGroup

	// cancelFuncs maps the childNames to their goroutine-cancelling functions.
	cancelFuncs := make(map[string]func())

	// Defer cleanup (cancel all active goroutines).
	defer func() {
		for _, cancel := range cancelFuncs {
			cancel()
		}
		wg.Wait()
	}()

	dirEntries, err := os.ReadDir(name)
	if err != nil {
		wgoCmd.Logger.Println(err)
		return
	}
	for _, dirEntry := range dirEntries {
		childName := filepath.Join(name, dirEntry.Name())
		isDir := dirEntry.IsDir()
		if isDir {
			if !wgoCmd.matchDir(childName) {
				continue
			}
		} else {
			if !wgoCmd.matchFile(childName) {
				continue
			}
		}
		ctx, cancel := context.WithCancel(ctx)
		cancelFuncs[childName] = cancel
		if isDir {
			wg.Add(1)
			go func() {
				wg.Done()
				wgoCmd.Logger.Println("WATCH", childName)
				wgoCmd.pollDirectory(ctx, childName, events)
			}()
		} else {
			wg.Add(1)
			go func() {
				defer wg.Done()
				wgoCmd.pollFile(ctx, childName, events)
			}()
		}
	}

	// seen tracks which childNames we have already seen. We are declaring it
	// outside the loop instead of inside the loop so that we can reuse the
	// map.
	seen := make(map[string]bool)

	for {
		for childName := range seen {
			delete(seen, childName)
		}
		time.Sleep(wgoCmd.PollDuration)
		err := ctx.Err()
		if err != nil {
			return
		}
		dirEntries, err := os.ReadDir(name)
		if err != nil {
			wgoCmd.Logger.Println(err)
			return
		}
		for _, dirEntry := range dirEntries {
			childName := filepath.Join(name, dirEntry.Name())
			isDir := dirEntry.IsDir()
			if isDir {
				if !wgoCmd.matchDir(childName) {
					continue
				}
			} else {
				if !wgoCmd.matchFile(childName) {
					continue
				}
			}
			seen[childName] = true
			_, ok := cancelFuncs[childName]
			if ok {
				continue
			}
			ctx, cancel := context.WithCancel(ctx)
			cancelFuncs[childName] = cancel
			if isDir {
				wg.Add(1)
				go func() {
					defer wg.Done()
					wgoCmd.Logger.Println("WATCH", childName)
					events <- fsnotify.Event{Name: childName, Op: fsnotify.Create}
					wgoCmd.pollDirectory(ctx, childName, events)
				}()
			} else {
				wg.Add(1)
				go func() {
					defer wg.Done()
					events <- fsnotify.Event{Name: childName, Op: fsnotify.Create}
					wgoCmd.pollFile(ctx, childName, events)
				}()
			}
		}
		// For the child items that no longer exist, cancel their goroutines.
		for childName, cancel := range cancelFuncs {
			if !seen[childName] {
				cancel()
				delete(cancelFuncs, childName)
			}
		}
	}
}

func (wgoCmd *WgoCmd) pollFile(ctx context.Context, name string, events chan<- fsnotify.Event) {
	fileInfo, err := os.Stat(name)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			wgoCmd.Logger.Println(err)
		}
		return
	}
	oldModTime := fileInfo.ModTime()
	oldSize := fileInfo.Size()
	for {
		time.Sleep(wgoCmd.PollDuration)
		err := ctx.Err()
		if err != nil {
			return
		}
		fileInfo, err := os.Stat(name)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				wgoCmd.Logger.Println(err)
			}
			return
		}
		newModTime := fileInfo.ModTime()
		newSize := fileInfo.Size()
		if newModTime != oldModTime || newSize != oldSize {
			events <- fsnotify.Event{Name: name, Op: fsnotify.Write}
		}
		oldModTime = newModTime
		oldSize = newSize
	}
}
