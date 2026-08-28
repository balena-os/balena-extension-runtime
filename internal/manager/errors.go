package manager

import "errors"

// ErrEngineUnavailable reports that the balena-engine socket could not be
// reached.
var ErrEngineUnavailable = errors.New("balena-engine unavailable")
