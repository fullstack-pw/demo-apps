// Package shared provides a set of common utilities for microservices
package shared

import (
	// Import subpackages for easy access
	_ "shared/connections"
	_ "shared/health"
	_ "shared/logging"
	_ "shared/server"
	_ "shared/tracing"
)

// Version is the current version of the shared library
const Version = "0.1.0"
