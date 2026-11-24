package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// A failed redirect must not keep the VM from starting: the init
// container ends well (main exits 0 after it), saying why.
func TestRedirect_AFailureIsLoggedNotFatal(t *testing.T) {
	logged := ""
	logf := func(format string, args ...any) { logged = fmt.Sprintf(format, args...) }
	redirect(func() error { return errors.New("operation not permitted") }, logf)
	if !strings.Contains(logged, "without the metadata service: operation not permitted") {
		t.Fatalf("logged %q", logged)
	}
	redirect(func() error { return nil }, logf)
	if !strings.Contains(logged, "installed") {
		t.Fatalf("logged %q", logged)
	}
}
