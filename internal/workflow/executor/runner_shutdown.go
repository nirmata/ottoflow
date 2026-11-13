/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import "time"

// Shutdown contract between the controller, which builds the runner Job, and the runner.
const (
	// RunnerTerminationGracePeriod is the runner pod's terminationGracePeriodSeconds.
	RunnerTerminationGracePeriod = 30 * time.Second
	// RunnerShutdownHardStop is how long after SIGTERM the runner force-exits.
	RunnerShutdownHardStop = 15 * time.Second
	// RunnerShutdownHardStopEnv carries RunnerShutdownHardStop to the runner as a Go duration. An env
	// var, not a flag: an older runner image ignores an unknown env var but exits on an unknown flag.
	RunnerShutdownHardStopEnv = "OTTOFLOW_SHUTDOWN_HARD_STOP"
)
