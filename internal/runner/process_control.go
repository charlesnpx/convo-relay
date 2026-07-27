package runner

import "errors"

var errGracefulStopUnsupported = errors.New(
	"graceful process stop is unsupported on Windows; use kill or stop --kill",
)

type stopProcessOperations struct {
	alive       func(int) bool
	requestStop func(int, bool) error
}
