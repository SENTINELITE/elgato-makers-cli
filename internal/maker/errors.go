package maker

import "errors"

// ErrAuthentication reports a rejected Maker session or organization access.
var ErrAuthentication = errors.New("Maker authentication or access rejected")
