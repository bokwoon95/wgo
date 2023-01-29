package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
)

func main() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "hello world")
	})
	http.HandleFunc("/crash", func(w http.ResponseWriter, r *http.Request) {
		fmt.Println("server crashed")
		os.Exit(1)
	})
	var err error
	var ln net.Listener
	for i := 0; i < 10; i++ {
		ln, err = net.Listen("tcp", "localhost:808"+strconv.Itoa(i))
		if err == nil {
			break
		}
	}
	if ln == nil {
		ln, err = net.Listen("tcp", "localhost:0")
		if err != nil {
			log.Fatal(err)
		}
	}
	fmt.Println("Listening on localhost:" + strconv.Itoa(ln.Addr().(*net.TCPAddr).Port))
	log.Fatal(http.Serve(ln, nil))
}
