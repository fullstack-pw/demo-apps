// Package shared provides a set of common utilities for microservices
package shared

import (
	// Import subpackages for easy access
	_ "fullstack.pw/shared/connections"
	_ "fullstack.pw/shared/health"
	_ "fullstack.pw/shared/logging"
	_ "fullstack.pw/shared/server"
	_ "fullstack.pw/shared/tracing"
)

// Version is the current version of the shared library
const Version = "0.1.0"
