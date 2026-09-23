package vllog

import (
	"fmt"
	"log"
	"os"
)

// cliLogger provides convenient methods for CLI logging without timestamps
type cliLogger struct {
	*log.Logger
}

// newCliLogger creates a new CLI logger that outputs to stdout with no formatting
func newCliLogger() *cliLogger {
	return &cliLogger{
		Logger: log.New(os.Stdout, "", 0),
	}
}

// Info prints an info message to stdout
func (c *cliLogger) Info(msg string, args ...any) {
	if len(args) > 0 {
		c.Printf(msg, args...)
	} else {
		c.Print(msg)
	}
}

// Error prints an error message to stderr
func (c *cliLogger) Error(msg string, args ...any) {
	formatted := msg
	if len(args) > 0 {
		formatted = fmt.Sprintf(msg, args...)
	}
	fmt.Fprintf(os.Stderr, "[error] %s\n", formatted)
}

// Default cLI logger instance
var cLI = newCliLogger()
