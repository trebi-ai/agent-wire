//go:build !unix && !windows

package proc

import (
	"os"
	"os/exec"
	"syscall"
)

// platform is empty off Unix/Windows; there is no group or job primitive.
type platform struct{}

func (p *platform) afterStart(cmd *exec.Cmd) error { return nil }
func (p *platform) afterWait()                     {}

// rejectShim: executable resolution is the platform's problem here.
// isShim reports whether the path is a command-line wrapper.
func isShim(string) bool { return false }

// sysProcAttr is a no-op on plan9/js/wasi.
func sysProcAttr() *syscall.SysProcAttr { return nil }

func processGroupID(pid int) int { return 0 }

func termSignal() os.Signal { return os.Interrupt }

func (p *Proc) signalGroup(sig os.Signal) {
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Signal(sig)
	}
}

func (p *Proc) killTreePlatform() { p.signalGroup(os.Kill) }

// StartToken has no portable source here.
func StartToken(pid int) string { return "" }

// Alive reports whether pid exists.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	_, err := os.FindProcess(pid)
	return err == nil
}

// KillGroup kills one process; there is no group primitive here.
func KillGroup(_, pid int) {
	if pid <= 0 {
		return
	}
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}
