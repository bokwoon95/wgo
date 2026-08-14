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
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
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

// noopLogger is a logger that discards all its input.
var noopLogger = log.New(io.Discard, "", 0)

// FileEvent is a wrapper around fsnotify.Event except it also includes an
// additional IsDir field to indicate if the file path is a directory.
type FileEvent struct {
	fsnotify.Event
	IsDir bool
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

	// Logger is used to log file events.
	Logger *log.Logger

	// ErrorLogger is used to log errors.
	ErrorLogger *log.Logger

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

	// Stdin is where commands get stdin input from (EnableStdin must be true).
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
		Roots:       []string{cwd},
		Logger:      noopLogger,
		ErrorLogger: noopLogger,
		Stdin:       os.Stdin,
		Stdout:      os.Stdout,
		Stderr:      os.Stderr,
		Debounce:    300 * time.Millisecond,
		ctx:         ctx,
	}
	var verbose bool
	wgoCmd.isRun = len(args) > 0 && args[0] == "run"
	if wgoCmd.isRun {
		args = args[1:]
	}

	// Parse flags.
	var debounce, poll string
	flagset := flag.NewFlagSet("", flag.ContinueOnError)
	if output, ok := ctx.Value("flagsetOutput").(io.Writer); ok {
		flagset.SetOutput(output)
	}
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
		prefix := "[wgo] "
		if wgoNumber > 1 {
			prefix = fmt.Sprintf("[wgo%d] ", wgoNumber)
		}
		wgoCmd.Logger = log.New(os.Stderr, prefix, 0)
		wgoCmd.ErrorLogger = log.New(os.Stderr, prefix, log.Lshortfile)
	}
	if debounce != "" {
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
		wgoCmd.executablePath = filepath.Join(tmpDir, "wgo_"+time.Now().Format("20060102150405")+"_"+strconv.Itoa(rand.Intn(9999))) + ".exe"
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

	// fileEvents is a channel that receives events from the watcher and from
	// polling.
	fileEvents := make(chan FileEvent)

	// watcher emits fsnotify.Event when files change.
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()

	// dirSet tracks which paths are directories. Used to check if a path is a
	// directory when fsnotify sends out a Rename or Remove event (we can't use
	// os.Stat() on the path since it no longer exists).
	//
	// Not needed when we are polling, since polling tracks the directory state
	// of the path internally.
	dirSet := make(map[string]struct{})

	// polledRoots is a list of the root directories we are polling.
	var polledRoots []*PolledFile

	// For each root, if we are polling add the root to the polledRoots slice.
	// If we are not polling, add the root to the fsnotify watcher.
	for _, root := range wgoCmd.Roots {
		if wgoCmd.PollDuration > 0 {
			polledFile, err := wgoCmd.newPolledDirectory(root)
			if err != nil {
				wgoCmd.ErrorLogger.Println(err)
				continue
			}
			polledRoots = append(polledRoots, polledFile)
		} else {
			wgoCmd.addDirsRecursively(watcher, root, dirSet)
		}
	}

	// For each polled root, spin off polling goroutines that send FileEvents
	// into the fileEvents channel.
	for _, polledRoot := range polledRoots {
		go wgoCmd.pollFile(wgoCmd.ctx, polledRoot, fileEvents)
	}

	// Spin off a goroutine that drains fsnotify.Events from the watcher and
	// sends it as FileEvents into the fileEvents channel.
	go func() {
		for event := range watcher.Events {
			// Only permit Create | Write | Remove | Rename events.
			if !event.Has(fsnotify.Create) && !event.Has(fsnotify.Write) && !event.Has(fsnotify.Remove) && !event.Has(fsnotify.Rename) {
				continue
			}

			// Check if the file path is a directory.
			var isDir bool
			if event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) {
				_, isDir = dirSet[event.Name]
				if isDir {
					delete(dirSet, event.Name)
					prefix := event.Name + string(filepath.Separator)
					for dir := range dirSet {
						if strings.HasPrefix(dir, prefix) {
							delete(dirSet, dir)
						}
					}
				}
			} else {
				fileInfo, err := os.Stat(event.Name)
				if err != nil {
					wgoCmd.ErrorLogger.Println(err)
					continue
				}
				isDir = fileInfo.IsDir()
				if isDir {
					dirSet[event.Name] = struct{}{}
					if event.Has(fsnotify.Create) {
						wgoCmd.addDirsRecursively(watcher, event.Name, dirSet)
					}
				} else {
					delete(dirSet, event.Name)
				}
			}

			// Emit the FileEvent.
			fileEvents <- FileEvent{
				Event: event,
				IsDir: isDir,
			}
		}
	}()

	// Timer is used to debounce events. Each event does not directly trigger a
	// reload, it only resets the timer. Only when the timer is allowed to
	// fully expire will the reload actually occur.
	timer := time.NewTimer(0)
	timer.Stop()
	defer timer.Stop()

	// Start a background job that continuously drains data from wgo.Stdin and
	// feeds it into stdinPipe (which is connected to the exec.Cmd currently
	// running). stdinPipe can be swapped out anytime when the exec.Cmd
	// changes, so access is guarded by a mutex.
	var stdinPipe io.WriteCloser
	var stdinPipeMutex sync.Mutex
	if wgoCmd.EnableStdin {
		go func() {
			p := make([]byte, 4096)
			for {
				n, err := wgoCmd.Stdin.Read(p)
				if n > 0 {
					stdinPipeMutex.Lock()
					if stdinPipe != nil {
						_, _ = stdinPipe.Write(p[:n])
					}
					stdinPipeMutex.Unlock()
				}
				if err == io.EOF {
					break
				}
				if err != nil {
					wgoCmd.ErrorLogger.Println(err)
					break
				}
			}
		}()
	}

	for restartCount := 0; ; restartCount++ {
	CMD_CHAIN:
		for i, args := range wgoCmd.ArgsList {
			if restartCount == 0 && wgoCmd.Postpone {
				for {
					select {
					case <-wgoCmd.ctx.Done():
						return nil
					case fileEvent := <-fileEvents:
						if wgoCmd.match(fileEvent) {
							timer.Reset(wgoCmd.Debounce)
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
			// If the user enabled stdin, feed wgoCmd.Stdin to the command's
			// Stdin by pointing stdinPipe at the cmd.
			if wgoCmd.EnableStdin {
				flushStdin(wgoCmd.Stdin)
				stdinPipeMutex.Lock()
				stdinPipe, err = cmd.StdinPipe()
				stdinPipeMutex.Unlock()
				if err != nil {
					return err
				}
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
				defer close(waitDone)
				cmdResult <- cmd.Wait()
			}()

			// Step 3: Wait for events in the event loop.
			for {
				select {
				case <-wgoCmd.ctx.Done():
					stop(cmd)
					<-waitDone
					return wgoCmd.ctx.Err()
				case err := <-cmdResult:
					if i == len(wgoCmd.ArgsList)-1 {
						if wgoCmd.Exit {
							return err
						}
						break
					}
					if err != nil {
						wgoCmd.ErrorLogger.Println(err)
						break
					}
					continue CMD_CHAIN
				case fileEvent := <-fileEvents:
					if wgoCmd.match(fileEvent) {
						timer.Reset(wgoCmd.Debounce) // Start the timer.
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
// wgoCmd.pollFile() instead, which does its own recursive polling.
func (wgoCmd *WgoCmd) addDirsRecursively(watcher *fsnotify.Watcher, dir string, dirSet map[string]struct{}) {
	roots := make(map[string]struct{})
	for _, root := range wgoCmd.Roots {
		roots[root] = struct{}{}
	}
	// Unwatching occurs when we hit the syscall.EMFILE error ("too many open
	// files"), and we need to unwatch so that we have some file descriptors
	// available for starting commands i.e. cmd.Start(). On macOS, creating a
	// timer for debouncing already consumes a file descriptor [1], and if
	// there are no file descriptors available it panics with a very confusing
	// stack trace (actually it just ran out of file descriptors).
	//
	// https://dzone.com/articles/go-servers-understanding-epoll-kqueue-netpoll
	unwatchFiles := func() {
		watchList := watcher.WatchList()
		sort.Strings(watchList)
		unwatchCount := 256
		if unwatchCount > len(watchList)/2 {
			unwatchCount = int(0.2 * float64(len(watchList)))
		}
		for i := len(watchList) - unwatchCount; i < len(watchList); i++ {
			watcher.Remove(watchList[i])
		}
		wgoCmd.Logger.Printf("ERROR too many open files (%d directories), not watching any more\n", len(watchList))
	}
	_ = filepath.WalkDir(dir, func(path string, dirEntry fs.DirEntry, err error) error {
		if err != nil {
			wgoCmd.ErrorLogger.Println(err)
			return nil
		}
		if !dirEntry.IsDir() {
			return nil
		}
		dirSet[path] = struct{}{}
		normalizedDir := filepath.ToSlash(path)
		_, isRoot := roots[path]
		if isRoot {
			err := watcher.Add(path)
			if err != nil {
				if errors.Is(err, syscall.EMFILE) {
					unwatchFiles()
					return fs.SkipAll
				}
				wgoCmd.ErrorLogger.Println(err)
				return fs.SkipDir
			}
			wgoCmd.Logger.Println("WATCH", normalizedDir)
			return nil
		}
		for _, root := range wgoCmd.Roots {
			if after, ok := strings.CutPrefix(path, root+string(filepath.Separator)); ok {
				normalizedDir = filepath.ToSlash(after)
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
				err := watcher.Add(path)
				if err != nil {
					if errors.Is(err, syscall.EMFILE) {
						unwatchFiles()
						return fs.SkipAll
					}
					wgoCmd.ErrorLogger.Println(err)
					return fs.SkipDir
				}
				wgoCmd.Logger.Println("WATCH", normalizedDir)
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
		err = watcher.Add(path)
		if err != nil {
			if errors.Is(err, syscall.EMFILE) {
				unwatchFiles()
				return fs.SkipAll
			}
			wgoCmd.ErrorLogger.Println(err)
			return fs.SkipDir
		}
		wgoCmd.Logger.Println("WATCH", normalizedDir)
		return nil
	})
}

// match checks if a given file or directory path should trigger a reload. The
// op string is provided only for logging purposes, it is not actually used.
func (wgoCmd *WgoCmd) match(fileEvent FileEvent) bool {
	normalizedFile := filepath.ToSlash(fileEvent.Name)
	normalizedDir := filepath.ToSlash(filepath.Dir(normalizedFile))
	for _, root := range wgoCmd.Roots {
		root += string(os.PathSeparator)
		if after, ok := strings.CutPrefix(fileEvent.Name, root); ok {
			normalizedFile = filepath.ToSlash(after)
			normalizedDir = filepath.ToSlash(filepath.Dir(normalizedFile))
			break
		}
	}
	if fileEvent.IsDir {
		normalizedDir = normalizedFile
	}
	for _, r := range wgoCmd.ExcludeDirRegexps {
		if r.MatchString(normalizedDir) {
			wgoCmd.Logger.Println("(skip)", fileEvent.Op.String(), normalizedFile)
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
			wgoCmd.Logger.Println("(skip)", fileEvent.Op.String(), normalizedFile)
			return false
		}
		if fileEvent.IsDir {
			wgoCmd.Logger.Println(fileEvent.Op.String(), normalizedFile)
			return true
		}
	}
	if fileEvent.IsDir {
		name := filepath.Base(normalizedDir)
		switch name {
		case ".git", ".hg", ".svn", ".idea", ".vscode", ".settings", "node_modules":
			wgoCmd.Logger.Println("(skip)", fileEvent.Op.String(), normalizedFile)
			return false
		}
		if strings.HasPrefix(name, ".") {
			wgoCmd.Logger.Println("(skip)", fileEvent.Op.String(), normalizedFile)
			return false
		}
		wgoCmd.Logger.Println(fileEvent.Op.String(), normalizedFile)
		return true
	}
	for _, r := range wgoCmd.ExcludeFileRegexps {
		if r.MatchString(normalizedFile) {
			wgoCmd.Logger.Println("(skip)", fileEvent.Op.String(), normalizedFile)
			return false
		}
	}
	for _, r := range wgoCmd.FileRegexps {
		if r.MatchString(normalizedFile) {
			wgoCmd.Logger.Println(fileEvent.Op.String(), normalizedFile)
			return true
		}
	}
	if wgoCmd.isRun {
		if strings.HasSuffix(fileEvent.Name, ".go") && !strings.HasSuffix(fileEvent.Name, "_test.go") {
			wgoCmd.Logger.Println(fileEvent.Op.String(), normalizedFile)
			return true
		}
		wgoCmd.Logger.Println("(skip)", fileEvent.Op.String(), normalizedFile)
		return false
	}
	if len(wgoCmd.FileRegexps) == 0 {
		wgoCmd.Logger.Println(fileEvent.Op.String(), normalizedFile)
		return true
	}
	wgoCmd.Logger.Println("(skip)", fileEvent.Op.String(), normalizedFile)
	return false
}

// PolledFile represents a file path being polled.
type PolledFile struct {
	// Path of the file.
	Path string

	// IsDir stores whether the file is a directory.
	IsDir bool

	// (Files only) Modification time of the file.
	ModTime time.Time

	// (Files only) Size of the file.
	Size int64

	// (Directories only) Map of the directory's children, keyed by the child
	// name (NOT their path).
	ChildMap map[string]*PolledFile

	// (Directories only) WaitGroup that tracks the active number children in
	// the ChildMap.
	ChildWaitGroup sync.WaitGroup

	// Cancel will cancel the context associated with the polling function,
	// causing it to return.
	Cancel context.CancelFunc
}

// newPolledDirectory creates a PolledFile from a given path (must be a directory).
func (wgoCmd *WgoCmd) newPolledDirectory(path string) (*PolledFile, error) {
	file := &PolledFile{
		Path:     path,
		IsDir:    true,
		ChildMap: make(map[string]*PolledFile),
	}
	dirEntries, err := os.ReadDir(path)
	if err != nil {
		wgoCmd.ErrorLogger.Println(err)
		return nil, err
	}
	for _, dirEntry := range dirEntries {
		childName := dirEntry.Name()
		childPath := filepath.Join(path, childName)
		if !dirEntry.IsDir() {
			fileInfo, err := os.Stat(childPath)
			if err != nil {
				wgoCmd.ErrorLogger.Println(err)
				continue
			}
			file.ChildMap[childName] = &PolledFile{
				Path:    childPath,
				IsDir:   false,
				ModTime: fileInfo.ModTime(),
				Size:    fileInfo.Size(),
			}
			continue
		}

		// normalizedDir is childPath with the matching root prefix trimmed
		// away.
		normalizedDir := filepath.ToSlash(childPath)
		for _, root := range wgoCmd.Roots {
			if after, ok := strings.CutPrefix(childPath, root+string(filepath.Separator)); ok {
				normalizedDir = filepath.ToSlash(after)
				break
			}
		}

		excluded := false
		for _, r := range wgoCmd.ExcludeDirRegexps {
			if r.MatchString(normalizedDir) {
				excluded = true
				break
			}
		}
		if excluded {
			continue
		}

		included := false
		for _, r := range wgoCmd.DirRegexps {
			if r.MatchString(normalizedDir) {
				included = true
				break
			}
		}
		if !included {
			switch filepath.Base(normalizedDir) {
			case ".git", ".hg", ".svn", ".idea", ".vscode", ".settings", "node_modules":
				continue
			}
			if strings.HasPrefix(filepath.Base(normalizedDir), ".") {
				continue
			}
		}

		child, err := wgoCmd.newPolledDirectory(childPath)
		if err != nil {
			wgoCmd.ErrorLogger.Println(err)
			return nil, err
		}
		file.ChildMap[childName] = child
	}
	wgoCmd.Logger.Println("POLL", filepath.ToSlash(file.Path))
	return file, nil
}

// pollFile polls a PolledFile for changes.
func (wgoCmd *WgoCmd) pollFile(ctx context.Context, file *PolledFile, fileEvents chan<- FileEvent) {
	// If the PolledFile is a normal file and not a directory, just poll it in
	// a loop.
	if !file.IsDir {
		for {
			// Sleep for PollDuration.
			time.Sleep(wgoCmd.PollDuration)

			// Check if we are canceled.
			err := ctx.Err()
			if err != nil {
				return
			}

			// Check if the file has changed.
			fileInfo, err := os.Stat(file.Path)
			if err != nil {
				wgoCmd.ErrorLogger.Println(err)
				continue
			}
			if fileInfo.ModTime() != file.ModTime || fileInfo.Size() != file.Size {
				fileEvents <- FileEvent{
					Event: fsnotify.Event{
						Name: file.Path,
						Op:   fsnotify.Write,
					},
				}
			}
			file.ModTime = fileInfo.ModTime()
			file.Size = fileInfo.Size()
		}
	}

	// If we reach here, it means the PolledFile is a directory (since a normal
	// file would be caught in the loop above).

	// Before we exit this polling function, make sure to cancel all its child
	// polling functions and wait for them to return.
	defer func() {
		for _, child := range file.ChildMap {
			child.Cancel()
		}
		file.ChildWaitGroup.Wait()
	}()

	// Spin off polling goroutines for each child.
	for _, child := range file.ChildMap {
		ctx, cancel := context.WithCancel(ctx)
		child.Cancel = cancel
		file.ChildWaitGroup.Add(1)
		go func() {
			defer file.ChildWaitGroup.Done()
			wgoCmd.pollFile(ctx, child, fileEvents)
		}()
	}

	// seen tracks which children we have seen during each polling iteration.
	seen := make(map[*PolledFile]struct{})

	for {
		// Sleep for PollDuration.
		time.Sleep(wgoCmd.PollDuration)

		// Check if we are canceled.
		err := ctx.Err()
		if err != nil {
			return
		}

		// Reset the children we have seen.
		clear(seen)

		// Loop over dirEntries of this directory.
		dirEntries, err := os.ReadDir(file.Path)
		if err != nil {
			wgoCmd.ErrorLogger.Println(err)
			continue
		}
		for _, dirEntry := range dirEntries {
			childName := dirEntry.Name()
			childPath := filepath.Join(file.Path, childName)

			// Check if the child already exists.
			child := file.ChildMap[childName]
			if child != nil {
				// If child.IsDir matches dirEntry.IsDir(), we're already
				// tracking this child. Mark as seen and continue.
				if child.IsDir == dirEntry.IsDir() {
					seen[child] = struct{}{}
					continue
				}
				// Otherwise, the child shares the same name as the dirEntry
				// but its IsDir has switched sides (file -> directory or
				// directory -> file) since we last checked it. Treat it as the
				// old child was removed, cancel its polling goroutine and
				// evict it from the ChildMap and emit a Remove event.
				child.Cancel()
				delete(file.ChildMap, childName)
				fileEvents <- FileEvent{
					Event: fsnotify.Event{
						Name: childPath,
						Op:   fsnotify.Remove,
					},
					IsDir: child.IsDir,
				}
			}

			// If we reach here, it means the current dirEntry did not exist
			// prior to this. Create a new child PolledFile based on whether or
			// not it is a directory.
			if dirEntry.IsDir() {
				child, err = wgoCmd.newPolledDirectory(childPath)
				if err != nil {
					wgoCmd.ErrorLogger.Println(err)
					continue
				}
			} else {
				fileInfo, err := os.Stat(childPath)
				if err != nil {
					wgoCmd.ErrorLogger.Println(err)
					continue
				}
				child = &PolledFile{
					Path:    childPath,
					IsDir:   false,
					ModTime: fileInfo.ModTime(),
					Size:    fileInfo.Size(),
				}
			}

			// Mark the child as seen, register it in the ChildMap and emit a
			// Create event.
			seen[child] = struct{}{}
			ctx, cancel := context.WithCancel(ctx)
			child.Cancel = cancel
			file.ChildMap[childName] = child
			fileEvents <- FileEvent{
				Event: fsnotify.Event{
					Name: childPath,
					Op:   fsnotify.Create,
				},
				IsDir: dirEntry.IsDir(),
			}

			// Start polling the newly-created child.
			file.ChildWaitGroup.Add(1)
			go func() {
				defer file.ChildWaitGroup.Done()
				wgoCmd.pollFile(ctx, child, fileEvents)
			}()
		}

		// For children in the ChildMap, if they were not seen in the last
		// iteration treat them as removed. Cancel the polling goroutine,
		// delete it from the ChildMap and emit a Remove event.
		for name, child := range file.ChildMap {
			if _, ok := seen[child]; ok {
				continue
			}
			child.Cancel()
			delete(file.ChildMap, name)
			fileEvents <- FileEvent{
				Event: fsnotify.Event{
					Name: child.Path,
					Op:   fsnotify.Remove,
				},
				IsDir: child.IsDir,
			}
		}
	}
}
