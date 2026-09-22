//go:build windows

package proc

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// platform carries the job object so KillTree terminates the whole process
// tree even when the child spawned helpers after start.
type platform struct {
	job windows.Handle
}

// ctrlBreak is the graceful stop request. It is deliberately not os.Interrupt:
// GenerateConsoleCtrlEvent delivers it to a process group, while
// os.Process.Signal can only send Kill on Windows.
const ctrlBreak = windows.CTRL_BREAK_EVENT

// afterStart assigns the child to a kill-on-close job object. Assignment can
// fail when the daemon itself already runs inside a job (CI, some terminals);
// that is not fatal, it only weakens tree-kill to root-process kill.
func (p *platform) afterStart(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return nil
	}
	handle, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		windows.CloseHandle(job)
		return nil
	}
	err = windows.AssignProcessToJobObject(job, handle)
	windows.CloseHandle(handle)
	if err != nil {
		windows.CloseHandle(job)
		return nil
	}
	p.job = job
	return nil
}

// afterWait releases the job handle; KILL_ON_JOB_CLOSE reaps any stragglers.
func (p *platform) afterWait() {
	if p.job != 0 {
		_ = windows.CloseHandle(p.job)
		p.job = 0
	}
}

// killTreePlatform terminates the whole job when one was assigned, else the
// root process alone.
func (p *Proc) killTreePlatform() {
	if p.platform.job != 0 {
		_ = windows.TerminateJobObject(p.platform.job, 1)
		return
	}
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
}

// rejectShim refuses npm .cmd/.bat wrappers: they re-parse the command line
// and a structured-protocol child must be a native executable.
func isShim(path string) bool {
	low := strings.ToLower(path)
	return strings.HasSuffix(low, ".cmd") || strings.HasSuffix(low, ".bat")
}

// sysProcAttr starts the child in its own console process group.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
}

func processGroupID(pid int) int { return pid }

// termSignalValue is the graceful stop request. It never reaches
// Process.Signal; signalGroup maps it to a console control event.
var termSignalValue = syscall.Signal(0xf)

func termSignal() os.Signal { return termSignalValue }

// signalGroup delivers a graceful stop to the child's console group and a
// hard stop to the whole job.
func (p *Proc) signalGroup(sig os.Signal) {
	if sig == termSignalValue {
		if p.pgid > 0 {
			// Fails when the daemon shares no console with the group; the
			// SIGKILL stage still runs after the flush window.
			_ = windows.GenerateConsoleCtrlEvent(ctrlBreak, uint32(p.pgid))
		}
		return
	}
	p.killTreePlatform()
}

// StartToken uses the process creation time as the positive-match token,
// mirroring lstart= on Unix.
func StartToken(pid int) string {
	if pid <= 0 {
		return ""
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	var created, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exit, &kernel, &user); err != nil {
		return ""
	}
	return time.Unix(0, created.Nanoseconds()).UTC().Format(time.RFC3339Nano)
}

// Alive reports whether pid exists.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	windows.CloseHandle(h)
	return true
}

// KillGroup kills a process and its children. A process this library did not
// spawn has no job handle here, so the OS walks the tree; a root-only kill is
// the fallback when taskkill is unavailable.
func KillGroup(_, pid int) {
	if pid <= 0 {
		return
	}
	if err := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).Run(); err == nil {
		return
	}
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}
