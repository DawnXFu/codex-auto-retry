//go:build !windows

package watchdog

import "os/exec"

type supervisorLock struct{}

func acquireSupervisorLock(string) (supervisorLocker, error) { return &supervisorLock{}, nil }
func (supervisorLock) Close() error                          { return nil }
func configureSupervisorWorker(*exec.Cmd)                    {}
