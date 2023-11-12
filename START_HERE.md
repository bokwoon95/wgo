This document describes how the codebase is organized. It is meant for people who are contributing to the codebase (or are just casually browsing).

Files are written in such a way that each successive file in the list below only depends on files that come before it. This makes it easy to rewrite the codebase from scratch file-by-file, complete with working tests at every step of the way. Please adhere to this file order when submitting pull requests.

- [**util_unix.go**](https://github.com/bokwoon95/wgo/blob/main/util_unix.go)
    - unix-specific `stop(cmd)` (which stops an \*exec.Cmd cleanly) and `joinArgs(args)` (which joins an args slice into a string that can be evaluated by bash).
- [**util_windows.go**](https://github.com/bokwoon95/wgo/blob/main/util_windows.go)
    - windows-specific `stop(cmd)` (which stops an \*exec.Cmd cleanly) and `joinArgs(args)` (which joins an args slice into a string that can be evaluated by powershell).
- [**wgo_cmd.go**](https://github.com/bokwoon95/wgo/blob/main/wgo_cmd.go)
    - `type WgoCmd struct`
    - `WgoCommand(ctx, args)`, which initializes a new WgoCmd. `WgoCommands(ctx, args)` instantiates a slice of WgoCmds.
    - `(*WgoCmd).Run()` runs the WgoCmd.
- [**main.go**](https://github.com/bokwoon95/wgo/blob/main/main.go)
    - `main()` instantiates a slice of WgoCmds from `os.Args` and runs them in parallel.

## Testing

Add tests if you add code.

To run tests, use:

```shell
$ go test . -race # -shuffle=on -coverprofile=coverage
```

PS: I noticed TestWgoCmd\_FileEvent() was consistently failing when running it on an ancient laptop, I've been using a faster laptop to circumvent the issue. If you're using a slow computer you might encounter the same thing. It's a very flaky test due to using time.Sleep, but I'm not sure how else to test it currently.
