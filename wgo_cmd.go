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
	"strconv"
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
	// precedence than later roots (used during file matching).
	//
	// Roots should use OS-specific file separators i.e. forward slash '/' on
	// Linux/macOS and backslash '\' on Windows. They will be normalized to
	// forward slashes later during matching.
	//
	// As a rule of thumb, this file should not import the package "path". It
	// should only use functions in the package "path/filepath".
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

	// Debounce duration for file events.
	Debounce time.Duration

	// If Postpone is true, WgoCmd will postpone the first execution of the
	// command(s) until a file is modified.
	Postpone bool

	// PollDuration is the duration at which we poll for events. The zero value
	// means no polling.
	PollDuration time.Duration

	ctx            context.Context
	isRun          bool   // Whether the command is `wgo run`.
	executablePath string // The output path of the `go build` executable.
}

// WgoCommands instantiates a slices of WgoCmds. Each "::" separator followed
// by "wgo" indicates a new WgoCmd.
func WgoCommands(ctx context.Context, args []string) ([]*WgoCmd, error) {
	var wgoCmds []*WgoCmd
	i, j, wgoNumber := 1, 1, 1
	for j < len(args) {
		if args[j] != "::" || j+1 >= len(args) || args[j+1] != "wgo" {
			j++
			continue
		}
		wgoCmd, err := WgoCommand(ctx, wgoNumber, args[i:j])
		if err != nil {
			if wgoNumber > 1 {
				return nil, fmt.Errorf("[wgo%d] %w", wgoNumber, err)
			}
			return nil, fmt.Errorf("[wgo] %w", err)
		}
		wgoCmds = append(wgoCmds, wgoCmd)
		i, j, wgoNumber = j+2, j+2, wgoNumber+1
	}
	if j > i {
		wgoCmd, err := WgoCommand(ctx, wgoNumber, args[i:j])
		if err != nil {
			if wgoNumber > 1 {
				return nil, fmt.Errorf("[wgo%d] %w", wgoNumber, err)
			}
			return nil, fmt.Errorf("[wgo] %w", err)
		}
		wgoCmds = append(wgoCmds, wgoCmd)
	}
	return wgoCmds, nil
}

// WgoCommand instantiates a new WgoCmd. Each "::" separator indicates a new
// chained command.
func WgoCommand(ctx context.Context, wgoNumber int, args []string) (*WgoCmd, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	wgoCmd := WgoCmd{
		Roots:  []string{cwd},
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
	flagset.BoolVar(&wgoCmd.Postpone, "postpone", false, "Postpone the first execution of the command until a file is modified.")
	flagset.StringVar(&poll, "poll", "", "How often to poll for file changes. Zero or no value means no polling.")
	flagset.Func("root", "Specify an additional root directory to watch. Can be repeated.", func(value string) error {
		root, err := filepath.Abs(value)
		if err != nil {
			return err
		}
		wgoCmd.Roots = append(wgoCmd.Roots, root)
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
		if wgoNumber > 1 {
			wgoCmd.Logger = log.New(os.Stderr, fmt.Sprintf("[wgo%d] ", wgoNumber), 0)
		} else {
			wgoCmd.Logger = log.New(os.Stderr, "[wgo] ", 0)
		}
	}
	if debounce == "" {
		wgoCmd.Debounce = 300 * time.Millisecond
	} else {
		wgoCmd.Debounce, err = time.ParseDuration(debounce)
		if err != nil {
			return nil, fmt.Errorf("-debounce: %w", err)
		}
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
		wgoCmd.executablePath = filepath.Join(tmpDir, "wgo_"+time.Now().Format("20060102150405")+"_"+strconv.Itoa(rand.Intn(5000)))
		if runtime.GOOS == "windows" {
			wgoCmd.executablePath += ".exe"
		}
		buildArgs := []string{"go", "build", "-o", wgoCmd.executablePath}
		buildArgs = append(buildArgs, strFlagValues...)
		for i, ok := range boolFlagValues {
			if ok {
				buildArgs = append(buildArgs, "-"+boolFlagNames[i])
			}
		}
		buildArgs = append(buildArgs, flagArgs[0])
		runArgs := []string{wgoCmd.executablePath}
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
	for i := range wgoCmd.Roots {
		var err error
		wgoCmd.Roots[i], err = filepath.Abs(wgoCmd.Roots[i])
		if err != nil {
			return err
		}
	}
	if wgoCmd.executablePath != "" {
		defer os.Remove(wgoCmd.executablePath)
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	// events channel will receive events either from the watcher or from
	// polling.
	//
	// I would really prefer to use the watcher.Events channel directly instead
	// of creating an intermediary channel that aggregates from both sources,
	// but for some reason that will set off the race detector during tests so
	// I have to use a separate channel :(.
	events := make(chan fsnotify.Event)
	go func() {
		for {
			event := <-watcher.Events
			events <- event
		}
	}()
	defer watcher.Close()
	for _, root := range wgoCmd.Roots {
		if wgoCmd.PollDuration > 0 {
			wgoCmd.Logger.Println("POLL", filepath.ToSlash(root))
			go wgoCmd.pollDirectory(wgoCmd.ctx, root, events)
		} else {
			wgoCmd.addDirsRecursively(watcher, root)
		}
	}
	// Timer is used to debounce events. Each event does not directly trigger a
	// reload, it only resets the timer. Only when the timer is allowed to
	// fully expire will the reload actually occur.
	timer := time.NewTimer(0)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	for restartCount := 0; ; restartCount++ {
	CMD_CHAIN:
		for i, args := range wgoCmd.ArgsList {
			if restartCount == 0 && wgoCmd.Postpone {
				for {
					select {
					case <-wgoCmd.ctx.Done():
						return nil
					case event := <-events:
						if !event.Has(fsnotify.Create) && !event.Has(fsnotify.Write) {
							continue
						}
						fileinfo, err := os.Stat(event.Name)
						if err != nil {
							continue
						}
						if fileinfo.IsDir() {
							if event.Has(fsnotify.Create) && wgoCmd.PollDuration == 0 {
								wgoCmd.addDirsRecursively(watcher, event.Name)
							}
						} else {
							if wgoCmd.match(event.Op.String(), event.Name) {
								timer.Reset(wgoCmd.Debounce)
							}
						}
					case <-timer.C:
						break CMD_CHAIN
					}
				}
			}
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
			wgoCmd.Logger.Println("EXECUTING", cmd.String())
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
				case event := <-events:
					if !event.Has(fsnotify.Create) && !event.Has(fsnotify.Write) {
						continue
					}
					fileinfo, err := os.Stat(event.Name)
					if err != nil {
						continue
					}
					if fileinfo.IsDir() {
						if event.Has(fsnotify.Create) && wgoCmd.PollDuration == 0 {
							wgoCmd.addDirsRecursively(watcher, event.Name)
						}
					} else {
						if wgoCmd.match(event.Op.String(), event.Name) {
							timer.Reset(wgoCmd.Debounce) // Start the timer.
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
//
// If we are polling (i.e. PollDuration > 0), do not call this method. Call
// wgoCmd.pollDirectory() instead, which does its own recursive polling.
func (wgoCmd *WgoCmd) addDirsRecursively(watcher *fsnotify.Watcher, dir string) {
	roots := make(map[string]struct{})
	for _, root := range wgoCmd.Roots {
		roots[root] = struct{}{}
	}
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		normalizedDir := filepath.ToSlash(path)
		_, isRoot := roots[path]
		if isRoot {
			wgoCmd.Logger.Println("WATCH", normalizedDir)
			watcher.Add(path)
			return nil
		}
		for _, root := range wgoCmd.Roots {
			if strings.HasPrefix(path, root+string(filepath.Separator)) {
				normalizedDir = filepath.ToSlash(strings.TrimPrefix(path, root+string(filepath.Separator)))
				break
			}
		}
		for _, r := range wgoCmd.ExcludeDirRegexps {
			if r.MatchString(normalizedDir) {
				return filepath.SkipDir
			}
		}
		for _, r := range wgoCmd.DirRegexps {
			if r.MatchString(normalizedDir) {
				wgoCmd.Logger.Println("WATCH", normalizedDir)
				watcher.Add(path)
				return nil
			}
		}
		name := filepath.Base(path)
		switch name {
		case ".git", ".hg", ".svn", ".idea", ".vscode", ".settings", "node_modules":
			return filepath.SkipDir
		}
		if strings.HasPrefix(name, ".") {
			return filepath.SkipDir
		}
		wgoCmd.Logger.Println("WATCH", normalizedDir)
		watcher.Add(path)
		return nil
	})
}

// match checks if a given file path should trigger a reload. The op string is
// provided only for logging purposes, it is not actually used.
func (wgoCmd *WgoCmd) match(op string, path string) bool {
	normalizedFile := filepath.ToSlash(path)
	normalizedDir := filepath.ToSlash(filepath.Dir(normalizedFile))
	for _, root := range wgoCmd.Roots {
		root += string(os.PathSeparator)
		if strings.HasPrefix(path, root) {
			normalizedFile = filepath.ToSlash(strings.TrimPrefix(path, root))
			normalizedDir = filepath.ToSlash(filepath.Dir(normalizedFile))
			break
		}
	}
	for _, r := range wgoCmd.ExcludeDirRegexps {
		if r.MatchString(normalizedDir) {
			wgoCmd.Logger.Println("(skip)", op, normalizedFile)
			return false
		}
	}
	if len(wgoCmd.DirRegexps) > 0 {
		matched := false
		for _, r := range wgoCmd.DirRegexps {
			if r.MatchString(normalizedDir) {
				matched = true
				break
			}
		}
		if !matched {
			wgoCmd.Logger.Println("(skip)", op, normalizedFile)
			return false
		}
	}
	for _, r := range wgoCmd.ExcludeFileRegexps {
		if r.MatchString(normalizedFile) {
			wgoCmd.Logger.Println("(skip)", op, normalizedFile)
			return false
		}
	}
	for _, r := range wgoCmd.FileRegexps {
		if r.MatchString(normalizedFile) {
			wgoCmd.Logger.Println(op, normalizedFile)
			return true
		}
	}
	if wgoCmd.isRun {
		if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			wgoCmd.Logger.Println(op, normalizedFile)
			return true
		}
		wgoCmd.Logger.Println("(skip)", op, normalizedFile)
		return false
	}
	if len(wgoCmd.FileRegexps) == 0 {
		wgoCmd.Logger.Println(op, normalizedFile)
		return true
	}
	wgoCmd.Logger.Println("(skip)", op, normalizedFile)
	return false
}

// pollDirectory polls a given directory path (recursively) for changes.
func (wgoCmd *WgoCmd) pollDirectory(ctx context.Context, path string, events chan<- fsnotify.Event) {
	// wg tracks the number of active goroutines.
	var wg sync.WaitGroup

	// cancelFuncs maps names to their goroutine-cancelling functions.
	cancelFuncs := make(map[string]func())

	// Defer cleanup.
	defer func() {
		for _, cancel := range cancelFuncs {
			cancel()
		}
		wg.Wait()
	}()

	dirEntries, err := os.ReadDir(path)
	if err != nil {
		wgoCmd.Logger.Println(err)
		return
	}
	for _, dirEntry := range dirEntries {
		name := dirEntry.Name()
		ctx, cancel := context.WithCancel(ctx)
		cancelFuncs[name] = cancel
		if dirEntry.IsDir() {
			match := func() bool {
				dir := filepath.Join(path, name)
				normalizedDir := filepath.ToSlash(dir)
				for _, root := range wgoCmd.Roots {
					if strings.HasPrefix(dir, root+string(filepath.Separator)) {
						normalizedDir = filepath.ToSlash(strings.TrimPrefix(dir, root+string(filepath.Separator)))
						break
					}
				}
				for _, r := range wgoCmd.ExcludeDirRegexps {
					if r.MatchString(normalizedDir) {
						return false
					}
				}
				for _, r := range wgoCmd.DirRegexps {
					if r.MatchString(normalizedDir) {
						wgoCmd.Logger.Println("POLL", normalizedDir)
						return true
					}
				}
				name := filepath.Base(normalizedDir)
				switch name {
				case ".git", ".hg", ".svn", ".idea", ".vscode", ".settings", "node_modules":
					return false
				}
				if strings.HasPrefix(name, ".") {
					return false
				}
				wgoCmd.Logger.Println("POLL", normalizedDir)
				return true
			}()
			if match {
				wg.Add(1)
				go func() {
					defer wg.Done()
					wgoCmd.pollDirectory(ctx, filepath.Join(path, name), events)
				}()
			}
		} else {
			wg.Add(1)
			go func() {
				defer wg.Done()
				wgoCmd.pollFile(ctx, filepath.Join(path, name), events)
			}()
		}
	}

	// seen tracks which names we have already seen. We are declaring it
	// outside the loop instead of inside the loop so that we can reuse the
	// map.
	seen := make(map[string]bool)

	for {
		for name := range seen {
			delete(seen, name)
		}
		time.Sleep(wgoCmd.PollDuration)
		err := ctx.Err()
		if err != nil {
			return
		}
		dirEntries, err := os.ReadDir(path)
		if err != nil {
			continue
		}
		for _, dirEntry := range dirEntries {
			name := dirEntry.Name()
			seen[name] = true
			_, ok := cancelFuncs[name]
			if ok {
				continue
			}
			ctx, cancel := context.WithCancel(ctx)
			cancelFuncs[name] = cancel
			if dirEntry.IsDir() {
				wg.Add(1)
				go func() {
					defer wg.Done()
					events <- fsnotify.Event{Name: filepath.Join(path, name), Op: fsnotify.Create}
					wgoCmd.pollDirectory(ctx, filepath.Join(path, name), events)
				}()
			} else {
				wg.Add(1)
				go func() {
					defer wg.Done()
					events <- fsnotify.Event{Name: filepath.Join(path, name), Op: fsnotify.Create}
					wgoCmd.pollFile(ctx, filepath.Join(path, name), events)
				}()
			}
		}
		// For names that no longer exist, cancel their goroutines.
		for name, cancel := range cancelFuncs {
			if !seen[name] {
				cancel()
				delete(cancelFuncs, name)
			}
		}
	}
}

// pollFile polls an individual file for changes.
func (wgoCmd *WgoCmd) pollFile(ctx context.Context, path string, events chan<- fsnotify.Event) {
	fileInfo, err := os.Stat(path)
	if err != nil {
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
		fileInfo, err := os.Stat(path)
		if err != nil {
			continue
		}
		newModTime := fileInfo.ModTime()
		newSize := fileInfo.Size()
		if newModTime != oldModTime || newSize != oldSize {
			events <- fsnotify.Event{Name: path, Op: fsnotify.Write}
		}
		oldModTime = newModTime
		oldSize = newSize
	}
}
