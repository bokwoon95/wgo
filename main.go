package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

const helptext = `Usage:
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
`

func main() {
	if len(os.Args) == 1 {
		fmt.Print(helptext)
		return
	}

	userInterrupt := make(chan os.Signal, 1)
	signal.Notify(userInterrupt, syscall.SIGTERM, syscall.SIGINT)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-userInterrupt // Soft interrupt.
		cancel()
		<-userInterrupt // Hard interrupt.
		os.Exit(1)
	}()

	// Construct the list of WgoCmds from os.Args.
	wgoCmds, err := WgoCommands(ctx, os.Args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		log.Fatal(err)
	}

	// Run the WgoCmds in parallel.
	results := make(chan error, len(wgoCmds))
	var wg sync.WaitGroup
	for _, wgoCmd := range wgoCmds {
		wgoCmd := wgoCmd
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- wgoCmd.Run()
		}()
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	// Wait for results.
	ok := true
	for err := range results {
		if err != nil {
			fmt.Println(err)
			ok = false
		}
	}
	if !ok {
		os.Exit(1)
	}
}
