package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

func main() {
	fmt.Printf("User Input: ")

	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		cmd := strings.TrimSpace(scanner.Text())

		fmt.Printf("Received: %s\n", cmd)
		fmt.Printf("User Input: ")
	}
}
