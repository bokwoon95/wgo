package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
)

func main() {
	scanner := bufio.NewScanner(os.Stdin)
	i := 0
	for scanner.Scan() {
		i++
		fmt.Fprintln(os.Stderr, strconv.Itoa(i)+": "+scanner.Text())
	}
}
