// hash-password prints the scrypt hash to store as
// DASHBOARD_ADMIN_PASSWORD_HASH in the vault. What `npm run hash-password`
// printed for the TS service; the format is identical and cross-verifies.
package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/admin"
)

func main() {
	args := os.Args[1:]
	var password string
	if len(args) == 1 {
		password = args[0]
	} else {
		fmt.Fprint(os.Stderr, "Password: ")
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		password = strings.TrimRight(line, "\r\n")
	}
	if password == "" {
		fmt.Fprintln(os.Stderr, "empty password")
		os.Exit(1)
	}
	hash, err := admin.HashPassword(password)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(hash)
}
