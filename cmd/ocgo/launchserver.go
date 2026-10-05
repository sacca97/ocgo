package main

import (
	"os"
	"path/filepath"
	"strconv"
)

func launchesDir() string     { return filepath.Join(configDir(), "launches") }
func launchOwnerFile() string { return filepath.Join(configDir(), "launch-owner") }

// acquireLaunchServer makes sure the proxy is running for a launched tool and
// returns a release function. Several launches can share one proxy: the
// launch that started it does not stop it while other launches are alive, and
// the last launch to exit stops it.
func acquireLaunchServer(base string) (func(), error) {
	if err := os.MkdirAll(launchesDir(), 0755); err != nil {
		return nil, err
	}
	mine := filepath.Join(launchesDir(), strconv.Itoa(os.Getpid()))
	if err := os.WriteFile(mine, nil, 0644); err != nil {
		return nil, err
	}
	cmd, err := startLaunchServer(base)
	if err != nil {
		os.Remove(mine)
		return nil, err
	}
	if cmd != nil {
		_ = os.WriteFile(launchOwnerFile(), []byte(strconv.Itoa(cmd.Process.Pid)), 0644)
		go func() { _ = cmd.Wait() }()
	}
	return func() {
		os.Remove(mine)
		if entries, err := os.ReadDir(launchesDir()); err == nil {
			for _, e := range entries {
				pid, err := strconv.Atoi(e.Name())
				if err != nil || !processAlive(pid) {
					os.Remove(filepath.Join(launchesDir(), e.Name()))
					continue
				}
				return // another launch still uses the proxy
			}
		}
		b, err := os.ReadFile(launchOwnerFile())
		if err != nil {
			return // proxy was started outside of a launch; leave it running
		}
		os.Remove(launchOwnerFile())
		if pid, err := strconv.Atoi(string(b)); err == nil {
			if p, err := os.FindProcess(pid); err == nil {
				_ = p.Kill()
			}
		}
		_ = os.Remove(pidFile())
	}, nil
}
