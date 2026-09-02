package repeat

import "errors"

// Sentinel errors are part of the public contract.
//
// Front-ends map them to exit codes.
// Callers use errors.Is to branch.
// Add new entries deliberately.
var (
	// ErrNotFound means a requested job or run does not exist.
	ErrNotFound = errors.New("repeat: not found")

	// ErrConflict means an operation conflicts with current state.
	ErrConflict = errors.New("repeat: conflict")

	// ErrDaemonUp means another daemon instance is already running.
	ErrDaemonUp = errors.New("repeat: daemon already running")

	// ErrInvalidCron means a cron expression is not accepted.
	ErrInvalidCron = errors.New("repeat: invalid cron expression")

	// ErrInvalidTime means a one-shot time expression is invalid.
	ErrInvalidTime = errors.New("repeat: invalid time")

	// ErrInvalidSpec means a job spec failed validation.
	ErrInvalidSpec = errors.New("repeat: invalid job spec")

	// ErrUnsupportedOS means the requested supervisor operation is unavailable.
	ErrUnsupportedOS = errors.New("repeat: unsupported OS")
)
