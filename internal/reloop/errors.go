package reloop

import "errors"

// Sentinel errors are part of the public contract.
//
// Front-ends map them to exit codes.
// Callers use errors.Is to branch.
// Add new entries deliberately.
var (
	// ErrNotFound means a requested job or run does not exist.
	ErrNotFound = errors.New("reloop: not found")

	// ErrConflict means an operation conflicts with current state.
	ErrConflict = errors.New("reloop: conflict")

	// ErrDaemonUp means another daemon instance is already running.
	ErrDaemonUp = errors.New("reloop: daemon already running")

	// ErrInvalidCron means a cron expression is not accepted.
	ErrInvalidCron = errors.New("reloop: invalid cron expression")

	// ErrInvalidTime means a one-shot time expression is invalid.
	ErrInvalidTime = errors.New("reloop: invalid time")

	// ErrInvalidSpec means a job spec failed validation.
	ErrInvalidSpec = errors.New("reloop: invalid job spec")

	// ErrUnsupportedOS means the requested supervisor operation is unavailable.
	ErrUnsupportedOS = errors.New("reloop: unsupported OS")
)
