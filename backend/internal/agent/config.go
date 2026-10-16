// Package agent holds the agent runner and its building blocks.
package agent

import (
	"errors"
	"time"
)

// Config holds the runner's limits.
type Config struct {
	MaxSteps     int
	MaxRuns      int
	MaxTokens    int
	RequeueDelay time.Duration
	PausedDelay  time.Duration
	// ShutdownRequeueDelay is how long a job interrupted by shutdown waits
	// before it can be claimed again. It must be at least the executor's
	// approvals.Config.RetrySettle (default 2m): then an approved call cut
	// off by the shutdown is reconciled and retried, not reported unknown.
	ShutdownRequeueDelay time.Duration
	KillSwitch           bool
	Now                  func() time.Time
}

// DefaultConfig returns the default limits.
func DefaultConfig() Config {
	return Config{
		MaxSteps:     30,
		MaxRuns:      20,
		MaxTokens:    4096,
		RequeueDelay: 30 * time.Second,
		PausedDelay:  5 * time.Minute,
		// = the approvals.Config.RetrySettle default
		ShutdownRequeueDelay: 2 * time.Minute,
		Now:                  time.Now,
	}
}

// withDefaults fills zero values with the defaults.
func (c Config) withDefaults() Config {
	d := DefaultConfig()
	if c.MaxSteps == 0 {
		c.MaxSteps = d.MaxSteps
	}
	if c.MaxRuns == 0 {
		c.MaxRuns = d.MaxRuns
	}
	if c.MaxTokens == 0 {
		c.MaxTokens = d.MaxTokens
	}
	if c.RequeueDelay == 0 {
		c.RequeueDelay = d.RequeueDelay
	}
	if c.PausedDelay == 0 {
		c.PausedDelay = d.PausedDelay
	}
	if c.ShutdownRequeueDelay == 0 {
		c.ShutdownRequeueDelay = d.ShutdownRequeueDelay
	}
	if c.Now == nil {
		c.Now = d.Now
	}
	return c
}

// validate rejects negative limits.
func (c Config) validate() error {
	if c.MaxSteps < 0 || c.MaxRuns < 0 || c.MaxTokens < 0 || c.RequeueDelay < 0 || c.PausedDelay < 0 ||
		c.ShutdownRequeueDelay < 0 {
		return errors.New("agent: config values must not be negative")
	}
	return nil
}
