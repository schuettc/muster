package cli

import (
	"bytes"
	"errors"
	"io"
	"strings"
)

// Dispatch is a test helper with the shape the old private dispatcher had:
// it runs muster through the real NewApp().Dispatch and turns a non-zero exit
// into an error carrying what tools.App printed to stderr. Tests keep their
// one-writer form while exercising the path the binary actually takes.
func Dispatch(args []string, out io.Writer) error {
	var errw bytes.Buffer
	if code := NewApp().Dispatch(args, out, &errw); code != 0 {
		return errors.New(strings.TrimSpace(errw.String()))
	}
	return nil
}
