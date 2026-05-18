package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"
)

// runHashPassword is the `zeno hash-password` entrypoint. It prompts for a
// password and prints a bcrypt hash suitable for pasting into
// `auth.password_hash` in config.yaml. When stdin is not a TTY (CI, scripts,
// docker exec without -t) it reads a single line from stdin instead.
func runHashPassword(args []string) {
	os.Exit(hashPasswordMain(args, os.Stdin, os.Stdout, os.Stderr))
}

func hashPasswordMain(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hash-password", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cost := fs.Int("cost", bcrypt.DefaultCost, "bcrypt cost (4-31)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	password, err := readPassword(stdin, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "hash-password: %v\n", err)
		return 1
	}
	if len(password) == 0 {
		fmt.Fprintln(stderr, "hash-password: empty password")
		return 1
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), *cost)
	if err != nil {
		fmt.Fprintf(stderr, "hash-password: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, string(hash))
	return 0
}

func readPassword(stdin io.Reader, stderr io.Writer) (string, error) {
	if f, ok := stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(stderr, "Password: ")
		raw, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(stderr)
		if err != nil {
			return "", err
		}
		return string(raw), nil
	}
	reader := bufio.NewReader(stdin)
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
