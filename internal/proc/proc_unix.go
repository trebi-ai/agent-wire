//go:build unix

package proc

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// platform is empty on Unix; Setpgid already gives group control.
type platform struct{}

func (p *platform) afterStart(cmd *exec.Cmd) error { return nil }
func (p *platform) afterWait()                     {}

// rejectShim is Windows-only; Unix resolves shebang executables itself.
// isShim reports whether the path is a command-line wrapper. Unix has none.
func isShim(string) bool { return false }

// sysProcAttr puts the child in its own process group so a kill escalates to
// the whole tree (npm wrappers, shells, language servers).
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

func (p *Proc) killTreePlatform() { p.signalGroup(syscall.SIGKILL) }

// processGroupID is PID once Setpgid is in effect.
func processGroupID(pid int) int { return pid }

func termSignal() os.Signal { return syscall.SIGTERM }

// signalGroup targets the process group first (negative pid), then the
// process, so a child that changed its own group still dies.
func (p *Proc) signalGroup(sig os.Signal) {
	if s, ok := sig.(syscall.Signal); ok && p.pgid > 0 {
		if err := syscall.Kill(-p.pgid, s); err == nil {
			return
		}
	}
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Signal(sig)
	}
}

// StartToken returns an identity token for a live pid: its start time. A
// mismatched token means the pid was reused and is not ours.
func StartToken(pid int) string {
	if pid <= 0 {
		return ""
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=").Output()
	if err != nil {
		return ""
	}
	return strings.Join(strings.Fields(string(out)), " ")
}

// Alive reports whether pid exists.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// KillGroup SIGKILLs the process group, then the process. It works on a
// process this library did not spawn, which is what ledger reconcile needs.
func KillGroup(pgid, pid int) {
	if pgid > 0 {
		if syscall.Kill(-pgid, syscall.SIGKILL) == nil {
			return
		}
	}
	if pid > 0 {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}
