package auth

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

// HandleCLI processes "adduser", "removeuser", "listusers" subcommands.
// Returns (handled, exitCode). When handled is true the caller should exit
// with the returned code instead of starting the HTTP server.
func HandleCLI(args []string) (bool, int) {
	if len(args) < 1 {
		return false, 0
	}
	if err := LoadConfig(); err != nil {
		fmt.Fprintln(os.Stderr, "load config:", err)
		return true, 1
	}
	if err := LoadUsers(); err != nil {
		fmt.Fprintln(os.Stderr, "load users:", err)
		return true, 1
	}

	switch args[0] {
	case "adduser":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: server adduser <username> [password]")
			return true, 2
		}
		username := args[1]
		var pw string
		if len(args) >= 3 {
			pw = args[2]
		} else {
			var err error
			pw, err = promptPassword("Password: ")
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return true, 1
			}
			confirm, err := promptPassword("Confirm: ")
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return true, 1
			}
			if pw != confirm {
				fmt.Fprintln(os.Stderr, "passwords do not match")
				return true, 1
			}
		}
		if err := AddUser(username, pw); err != nil {
			fmt.Fprintln(os.Stderr, "add user:", err)
			return true, 1
		}
		fmt.Println("user added:", username)
		return true, 0

	case "removeuser":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: server removeuser <username>")
			return true, 2
		}
		if err := RemoveUser(args[1]); err != nil {
			fmt.Fprintln(os.Stderr, "remove user:", err)
			return true, 1
		}
		fmt.Println("user removed:", args[1])
		return true, 0

	case "listusers":
		names := ListUsernames()
		if len(names) == 0 {
			fmt.Println("(no local users)")
			return true, 0
		}
		for _, n := range names {
			fmt.Println(n)
		}
		return true, 0
	}

	return false, 0
}

func promptPassword(prompt string) (string, error) {
	fmt.Print(prompt)
	if term.IsTerminal(int(os.Stdin.Fd())) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	// Non-interactive (tests, CI): read a line.
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return "", errors.New("empty password")
	}
	return line, nil
}
