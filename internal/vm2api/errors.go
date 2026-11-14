package vm2api

import (
	"errors"
	"fmt"
)

// Error is returned for any non-2xx vm2api answer. Code is vm2api's
// error.code when present; Message is safe to show an end user only after the
// caller maps it (do not echo vm2api internals verbatim to tenants).
type Error struct {
	Status  int
	Code    string
	Message string
	Op      string
}

func (e *Error) Error() string {
	return fmt.Sprintf("vm2api %s: %d %s: %s", e.Op, e.Status, e.Code, e.Message)
}

// ErrUnavailable wraps transport failures (connect refused, timeout).
var ErrUnavailable = errors.New("vm2api unavailable")

func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// ErrorCode returns the upstream error code, including through wrapped errors.
func ErrorCode(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func IsStatus(err error, status int) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == status
}
