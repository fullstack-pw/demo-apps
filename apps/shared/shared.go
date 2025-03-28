// Package shared provides a set of common utilities for microservices
package shared

import (
	// Import subpackages for easy access
	_ "github.com/fullstack-pw/shared/connections"
	_ "github.com/fullstack-pw/shared/health"
	_ "github.com/fullstack-pw/shared/logging"
	_ "github.com/fullstack-pw/shared/server"
	_ "github.com/fullstack-pw/shared/tracing"
)

// Version is the current version of the shared library
const Version = "0.2.0"
