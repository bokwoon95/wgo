package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

var trapSignal = flag.Bool("trap-signal", false, "")

func main() {
	flag.Parse()
	fmt.Println("Waiting...")
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	if *trapSignal {
		// Block forever until the program is forcefully terminated or until an
		// interrupt signal is received.
		select {
		case <-sigs:
			fmt.Println("Interrupt received, graceful shutdown.")
		}
	}
}
