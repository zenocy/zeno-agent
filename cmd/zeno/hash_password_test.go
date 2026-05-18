package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

func TestHashPasswordMain_NonTTYStdin_ProducesValidHash(t *testing.T) {
	password := "correct horse battery staple"
	stdin := strings.NewReader(password + "\n")
	var stdout, stderr bytes.Buffer

	rc := hashPasswordMain([]string{"--cost", "4"}, stdin, &stdout, &stderr)
	require.Equal(t, 0, rc, "stderr: %s", stderr.String())

	hash := strings.TrimSpace(stdout.String())
	require.NotEmpty(t, hash)
	require.NoError(t, bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)))
	require.Error(t, bcrypt.CompareHashAndPassword([]byte(hash), []byte("wrong")))
}

func TestHashPasswordMain_EmptyInputFails(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := hashPasswordMain([]string{"--cost", "4"}, strings.NewReader("\n"), &stdout, &stderr)
	require.Equal(t, 1, rc)
	require.Contains(t, stderr.String(), "empty password")
}
