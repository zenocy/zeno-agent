package synth

import (
	"io"
	"os"
)

// openOSFile is a tiny indirection so tests can rely on the standard library
// without cluttering the runner_test.go with os imports.
func openOSFile(path string) (io.ReadCloser, error) {
	return os.Open(path)
}
