[![tests](https://github.com/bokwoon95/sq/actions/workflows/tests.yml/badge.svg?branch=main)](https://github.com/bokwoon95/wgo/actions)
[![Go Report Card](https://goreportcard.com/badge/github.com/bokwoon95/wgo)](https://goreportcard.com/report/github.com/bokwoon95/wgo)
[![Coverage Status](https://shields.io/coverallsCoverage/github/bokwoon95/wgo?branch=main)](https://coveralls.io/github/bokwoon95/wgo?branch=main)

<div align="center"><h1>wgo – watcher-go</h1></div>
<div align="center"><h4>Live reload for Go apps (and more)</h4></div>
<hr>

## Installation

You can [download](#download-the-latest-release) binaries from [the release page](https://github.com/bokwoon95/wgo/releases/latest), or use the Go command:

```shell
go install github.com/bokwoon95/wgo@latest
```

```text
Usage:
  wgo [FLAGS] <command> [ARGUMENTS...]
  wgo gcc -o main main.c
  wgo go build -o main main.go
  wgo -file .c gcc -o main main.c
  wgo -file=.go go build -o main main.go

  wgo run [FLAGS] [GO_BUILD_FLAGS] <package> [ARGUMENTS...]
  wgo run main.go
  wgo run -file .html main.go arg1 arg2 arg3
  wgo run -file .html . arg1 arg2 arg3
  wgo run -file=.css -file=.js -tags=fts5 ./cmd/my_project arg1 arg2 arg3

Pass in the -h flag to the wgo/wgo run to learn what flags there are i.e. wgo -h, wgo run -h

Core documentation resides at https://github.com/bokwoon95/wgo#quickstart
```

## Why this exists

Too many file watchers either force you to wrap your commands into strings, require config files or log tons of noisy output to your stdout. In contrast, `wgo` is [dead simple](#quickstart) and silent by default. The implementation is also really short, most of it resides in just two files ([wgo\_cmd.go](https://github.com/bokwoon95/wgo/blob/main/wgo_cmd.go) and [main.go](https://github.com/bokwoon95/wgo/blob/main/main.go)). You can read the entire codebase in one sitting, [start here](https://github.com/bokwoon95/wgo/blob/main/START_HERE.md).

It can be used like [`go run`](#wgo-run).

## Quickstart

*You already know how to use wgo*. Simply slap `wgo` in front of any command in order to have it rerun whenever a file changes.

```shell
# Run gcc.
$ gcc -Wall -o main main.c

# Run gcc whenever a file changes.
$ wgo gcc -Wall -o main main.c
```

### wgo run

`wgo run` behaves exactly like `go run` except it runs again the moment any Go file changes. This can be used to live-reload Go servers. `wgo run` accepts the same flags as `go run`.

By default `wgo run` only watches .go files. To include additional file types such as .html, use the [-file flag](#including-and-excluding-files).

```shell
# Run main.go.
$ go run main.go

# Run main.go whenever a .go file changes.
$ wgo run main.go

# Any flag that can be passed to `go run` can also be passed to `wgo run`.
$ wgo run -tags=fts5 -race -trimpath main.go
```

## Flags

`wgo`/`wgo run` take in additional flags. These flags must be passed in directly after `wgo`/`wgo run`, before invoking your command.

- [-file/-xfile](#including-and-excluding-files) - Include/exclude files.
- [-dir/-xdir](#including-and-excluding-directories) - Include/exclude directories.
- [-cd](#running-commands-in-a-different-directory) - Change to a different directory to run the commands.
- [-root](#specify-additional-root-directories-to-watch) - Specify additional root directories to watch.
- [-exit](#exit-when-the-last-command-exits) - Exit when the last command exits.
- [-stdin](#enable-stdin) - Enable stdin for the last command.
- [-verbose](#log-file-events) - Log file events.

## Advanced Usage

- [Chaining commands](#chaining-commands)
- [Clear terminal on restart](#clear-terminal-on-restart)
- [Running parallel wgo commands](#running-parallel-wgo-commands)
- [Debug Go code using GoLand or VSCode with wgo](#debug-go-code-using-goland-or-vscode-with-wgo)

## Including and excluding files

[*back to flags index*](#flags)

To include specific files eligible for triggering a reload, use the -file flag. It takes in a regex, and files whose paths (relative to the root directory) match the regex are included. You can provide multiple -file flags.

Path separators are always forward slash, even on Windows.

If no -file flag is provided, every file is included by default unless it is explicitly excluded by the -xfile flag.

```shell
# Run sass whenever an .scss file changes.
$ wgo -file .scss sass assets/styles.scss assets/styles.css

# Run main.go whenever a .go or .html or .css file changes.
$ wgo run -file .html -file .css main.go

# Run main.go when foo/bar/baz.go changes.
$ wgo run -file '^foo/bar/baz.go$' main.go
```

To exclude specific files, use the -xfile flag. It takes in a regex (like -file) but if it matches with a file path that file is excluded.

The -xfile flag takes higher precedence than the -file flag so you can include a large group of files using a -file flag and surgically ignore specific files from that group using an -xfile flag.

```shell
# `go test` writes to coverage.out, which counts as a changed file which triggers
# `go test` to run again which generates coverage.out again in an infinite loop.
# Avoid this by excluding any file matching 'coverage.out'.
$ wgo -xfile coverage.out go test . -race -coverprofile=coverage.out

# Probably better to specify `-file .go` in order to rerun `go test` only when
# .go files change.
$ wgo -file .go go test . -race -coverprofile=coverage.out
```

## Regex dot literals

The [-file](#including-and-excluding-files) flag takes in regexes like `.html` or `.css`.

```shell
$ wgo run -file .html -file .css main.go
```

Technically the dot `.` matches any character, but file extensions are such a common pattern that wgo includes a slight modification to the regex matching rules:

*Any dot `.` immediately followed by an alphabet `[a-zA-Z]` is treated as a dot literal i.e. `\.`*

So `.css` really means `\.css`, but `.*` still means `.*` because `*` is not an alphabet. If you really want to use the dot `.` wildcard followed by an alphabet, wrap it in (a bracket group) so that it is not immediately followed by an alphabet i.e. `(.)abc`.

## Including and excluding directories

[*back to flags index*](#flags)

To only watch specific directories, use the -dir flag. It takes in a regex, and directories whose paths (relative to the root directory) match the regex are included. You can provide multiple -dir flags.

Path separators are always forward slash, even on Windows.

```shell
# Run sass whenever an .scss file in the assets directory changes.
$ wgo -dir assets -file .scss sass assets/styles.scss assets/styles.css

# Run main.go whenever something in the foo directory or bar/baz directory changes.
$ wgo run -dir foo -dir bar/baz main.go

# Run main.go whenever something in the foo/bar/baz directory changes.
$ wgo run -dir '^foo/bar/baz$' main.go
```

To exclude specific directories, use the -xdir flag. It takes in a regex (like -dir) but if it matches with a directory path that directory is excluded from being watched.

```shell
# Run main.go whenever a file changes, ignoring any directory called node_modules.
$ wgo run -xdir node_modules main.go
```

In practice you don't have to exclude `node_modules` because it's already excluded by default (together with `.git`, `.hg`, `.svn`, `.idea`, `.vscode` and `.settings`). If you do want to watch any of those directories, you should explicitly include it with the -dir flag.

## Chaining commands

Commands can be chained using the `::` separator. Subsequent commands are executed only when the previous command succeeds.

```shell
# Run `make build` followed by `make run` whenever a file changes.
$ wgo make build :: make run

# Run `go build` followed by the built binary whenever a .go file changes.
$ wgo -file .go go build -o main main.go :: ./main

# Clear the screen with `clear` before running `go test`.
# Windows users should use `cls` instead of `clear`.
$ wgo -file .go clear :: go test . -race -coverprofile=coverage.out
```

### Escaping the command separator

Since `::` designates the command separator, if you actually need to pass in a `::` string an an argument to a command you should escape it by appending an extra `:` to it. So `::` is escaped to `:::`, `:::` is escaped to `::::`, and so on.

### Shell wrapping

Chained commands execute in their own independent environment and are not implicitly wrapped in a shell. This can be a problem if you want to use shell specific commands like `if-else` statements or if you want to pass data between commands. In this case you should explicitly wrap the command(s) in a shell using the form `sh -c '<command>'` (or `pwsh.exe -command '<command>'` if you're on Windows).

```shell
# bash: echo "passed" or "failed" depending on whether the program succeeds.
$ wgo -file .go go build -o main main.go :: bash -c 'if ./main; then echo "passed"; else echo "failed"; fi'

# powershell: echo "passed" or "failed" depending on whether the program succeeds.
$ wgo -file .go go build -o main main.go :: pwsh.exe -command './main; if ($LastExitCode -eq 0) { echo "passed" } else { echo "failed" }'
```

### Clear terminal on restart

You can chain the `clear` command (or the `cls` command if you're on Windows) so that the terminal is cleared before everything restarts. You will not be able to use the `wgo run` command, instead you'll have to use the `wgo` command as a general-purpose file watcher to rerun `go run main.go` when a .go file changes.

```shell
# Clears the screen.
$ clear

# When a .go file changes, clear the screen and run go run main.go.
$ wgo -file .go clear :: go run main.go

# If you're on Windows:
$ wgo -file .go cls :: go run main.go
```

## Running parallel wgo commands

If a [command separator `::`](#chaining-commands) is followed by `wgo`, a new `wgo` command is started (which runs in parallel).

```shell
# Run the echo commands sequentially.
$ wgo echo foo :: echo bar :: echo baz

# Run the echo commands in parallel.
$ wgo echo foo :: wgo echo bar :: wgo echo baz
```

This allows you to reload a server when .go files change, but also do things like rebuild .scss and .ts files whenever they change at the same time.

```shell
$ wgo run main.go \
    :: wgo -file .scss sass assets/styles.scss assets/styles.css \
    :: wgo -file .ts tsc 'assets/*.ts' --outfile assets/index.js
```

## Running commands in a different directory

[*back to flags index*](#flags)

If you want to run commands in a different directory from where `wgo` was invoked, used the -cd flag.

```shell
# Run main.go whenever a file changes.
$ wgo run main.go

# Run main.go from the 'app' directory whenever a file changes.
$ wgo run -cd app main.go
```

## Specify additional root directories to watch

[*back to flags index*](#flags)

By default, the root being watched is the current directory. You can watch additional roots with the -root flag. Note that the [-file/-xfile](#including-and-excluding-files) and [-dir/-xdir](#including-and-excluding-directories) filters apply equally across all roots.

```shell
# Run main.go whenever a file in the current directory or the parent directory changes.
$ wgo run -root .. main.go

# Run main.go whenever a file in the current directory or the /env_secrets directory changes.
$ wgo run -root /env_secrets main.go
```

You may also be interested in the [-cd flag](#running-commands-in-a-different-directory), which lets you watch a directory but run commands from a different directory.

## Exit when the last command exits

[*back to flags index*](#flags)

If the -exit flag is provided, `wgo` exits once the last command exits. This is useful if your program is something like a server which should block forever, so file changes can trigger a reload as usual but if the server ever dies on its own (e.g. a panic or log.Fatal) wgo should exit to signal to some supervisor process that the server has been terminated.

```shell
# Exit once main.go exits.
$ wgo run -exit main.go

# Exit once ./main exits.
$ wgo -exit -file .go go build -o main main.go :: ./main
```

## Enable stdin

[*back to flags index*](#flags)

By default, stdin to wgo is ignored. You can enable it for the last command using the -stdin flag. This is useful for reloading programs that read from stdin.

```shell
# Allow main.go to read from stdin.
$ wgo run -stdin main.go

# Only ./main (the last command) gets to read from stdin, `go build` does not get stdin.
$ wgo -stdin -file .go go build -o main main.go :: ./main
```

## Log file events

[*back to flags index*](#flags)

If the -verbose flag is provided, file events are logged.

Without -verbose:

```shell
$ wgo run ./server
Listening on localhost:8080
Listening on localhost:8080 # <-- file edited.
```

With -verbose:

```shell
$ wgo run -verbose ./server
[wgo] WATCH /Users/bokwoon/Documents/wgo/testdata
[wgo] WATCH args
[wgo] WATCH build_flags
[wgo] WATCH dir
[wgo] WATCH dir/foo
[wgo] WATCH dir/subdir
[wgo] WATCH dir/subdir/foo
[wgo] WATCH env
[wgo] WATCH file_event
[wgo] WATCH hello_world
[wgo] WATCH server
[wgo] WATCH signal
[wgo] WATCH stdin
Listening on localhost:8080
[wgo] CREATE server/main.go~
[wgo] CREATE server/main.go
[wgo] WRITE server/main.go
Listening on localhost:8080
```

## Debug Go code using GoLand or VSCode with wgo

You need to ensure the [delve debugger](https://github.com/go-delve/delve) is installed.

```shell
go install github.com/go-delve/delve/cmd/dlv@latest
```

### Start the delve debugger on port 2345 using wgo.

```shell
$ wgo -file .go go build -o my_binary_name . :: sh -c 'while true; do dlv exec my_binary_name --headless --listen :2345 --api-version 2; done'

# If you're on Windows:
$ wgo -file .go go build -o my_binary_name . :: pwsh.exe -command 'while (1) { dlv exec my_binary_name --headless --listen :2345 --api-version 2 }'
```

You should see something like this

```shell
$ wgo -file .go go build -o my_binary_name . :: sh -c 'while true; do dlv exec my_binary_name --headless --listen :2345 --api-version 2; done'
API server listening at: [::]:2345
2024-12-01T01:18:13+08:00 warning layer=rpc Listening for remote connections (connections are not authenticated nor encrypted)
```

### For GoLand users, add a new "Go Remote" configuration

In the menu bar, Click on Run > Edit Configurations > Add New Configuration > Go Remote. Then fill in these values.

```
Name: Attach Debugger
Host: localhost
Port: 2345
On disconnect: Stop remote Delve process
```

Click OK. Now you can add breakpoints and then click Debug using this configuration. [Make sure the Delve debugger server is first started!](#start-the-delve-debugger-on-port-2345-using-wgo)

### For VSCode users, add a new launch.json configuration

```json
{
  "version": "0.2.0",
  "configurations": [
    {
      "name": "Attach Debugger",
      "type": "go",
      "request": "attach",
      "mode": "remote",
      "host": "localhost",
      "port": 2345
    }
  ]
}
```

Save the launch.json file. Now you can add breakpoints and Start Debugging using this configuration. [Make sure the Delve debugger server is first started!](#start-the-delve-debugger-on-port-2345-using-wgo)

## Why should I use this over other file watchers?

Nothing! File watchers honestly all do the same things, if you find a file watcher that works for you there's no reason to change. Maybe wgo has a [lower bar to entry](#quickstart)? Or maybe you're allergic to unnecessary config files like I am. Or if you want a feature like [chained commands](#chaining-commands) or [parallel commands](#running-parallel-wgo-commands), consider using `wgo`.

## Contributing

See [START\_HERE.md](https://github.com/bokwoon95/wgo/blob/main/START_HERE.md).

## Download the latest release

[Release page](https://github.com/bokwoon95/wgo/releases/latest)

### Linux

[https://github.com/bokwoon95/wgo/releases/latest/download/wgo-linux](https://github.com/bokwoon95/wgo/releases/latest/download/wgo-linux)

```shell
curl --location --output wgo 'https://github.com/bokwoon95/wgo/releases/latest/download/wgo-linux'
```

### Linux (ARM)

[https://github.com/bokwoon95/wgo/releases/latest/download/wgo-linux-arm](https://github.com/bokwoon95/wgo/releases/latest/download/wgo-linux-arm)

```shell
curl --location --output wgo "https://github.com/bokwoon95/wgo/releases/latest/download/wgo-linux-arm"
```

### macOS

[https://github.com/bokwoon95/wgo/releases/latest/download/wgo-macos](https://github.com/bokwoon95/wgo/releases/latest/download/wgo-macos)

```shell
curl --location --output wgo "https://github.com/bokwoon95/wgo/releases/latest/download/wgo-macos"
```

### macOS (Apple Silicon)

[https://github.com/bokwoon95/wgo/releases/latest/download/wgo-macos-apple-silicon](https://github.com/bokwoon95/wgo/releases/latest/download/wgo-macos-apple-silicon)

```shell
curl --location --output wgo "https://github.com/bokwoon95/wgo/releases/latest/download/wgo-macos-apple-silicon"
```

### Windows

[https://github.com/bokwoon95/wgo/releases/latest/download/wgo-windows.exe](https://github.com/bokwoon95/wgo/releases/latest/download/wgo-windows.exe)

```bat
curl --location --output wgo.exe "https://github.com/bokwoon95/wgo/releases/latest/download/wgo-windows.exe"
```
