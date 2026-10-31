package main

import (
	"errors"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Uninstall runs an app's uninstaller and reports when it is done.
//
// Uninstallers often hand the work to another process and exit at once (NSIS
// copies itself to the temporary folder, MSI runs in its service), so the
// uninstaller and everything it starts run in a job object, and the run ends
// once no process of the job is left. A process left running on purpose, as
// a browser opening a feedback page, would keep the job alive: the run ends
// as well a little after the uninstaller itself has exited and the app's
// registry entry is gone.
type Uninstall struct {
	Done       chan struct{} // closed when the uninstall is over
	mainExited atomic.Bool
	err        error
}

// MainExited reports whether the program the command started has exited,
// which may be before its helpers have.
func (u *Uninstall) MainExited() bool { return u.mainExited.Load() }

// Err is why the uninstaller could not run, once Done is closed.
func (u *Uninstall) Err() error { return u.err }

// StartUninstall runs app's uninstaller.
func StartUninstall(app *App, quiet bool) (*Uninstall, error) {
	cmd := app.Command(quiet)
	if cmd == "" {
		return nil, errors.New("the app has no uninstall command")
	}
	u := &Uninstall{Done: make(chan struct{})}

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	process, inJob, err := createSuspendedIn(job, cmd)
	if err == nil && !inJob {
		// It runs, but only its own exit can be waited for.
		windows.CloseHandle(job)
		job = 0
	}
	if errors.Is(err, windows.ERROR_ELEVATION_REQUIRED) {
		// Without administrator rights, an uninstaller that asks for them
		// starts through UAC, outside the job: only it can be waited for.
		windows.CloseHandle(job)
		job = 0
		process, err = shellExecuteRunas(commandExe(cmd), commandArgs(cmd, commandExe(cmd)))
	}
	if err != nil {
		if job != 0 {
			windows.CloseHandle(job)
		}
		return nil, err
	}
	go u.wait(app, job, process)
	return u, nil
}

// createSuspendedIn starts cmd suspended, puts it in job and lets it run.
// CreateProcess finds the program of a command line whose path has spaces
// and no quotes, as many UninstallString values are.
func createSuspendedIn(job windows.Handle, cmd string) (process windows.Handle, inJob bool, err error) {
	line, err := windows.UTF16PtrFromString(cmd)
	if err != nil {
		return 0, false, err
	}
	si := &windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfo{}))}
	var pi windows.ProcessInformation
	if err := windows.CreateProcess(nil, line, nil, nil, false,
		windows.CREATE_SUSPENDED|windows.CREATE_UNICODE_ENVIRONMENT, nil, nil, si, &pi); err != nil {
		return 0, false, err
	}
	defer windows.CloseHandle(pi.Thread)
	inJob = windows.AssignProcessToJobObject(job, pi.Process) == nil
	windows.ResumeThread(pi.Thread)
	return pi.Process, inJob, nil
}

func (u *Uninstall) wait(app *App, job, process windows.Handle) {
	defer close(u.Done)
	defer windows.CloseHandle(process)
	if job != 0 {
		defer windows.CloseHandle(job)
	}
	var goneSince time.Time
	for {
		if !u.mainExited.Load() {
			if ev, _ := windows.WaitForSingleObject(process, 250); ev == windows.WAIT_OBJECT_0 {
				u.mainExited.Store(true)
			}
			continue
		}
		active := activeProcesses(job)
		if active == 0 {
			return
		}
		if app.StillInstalled() {
			goneSince = time.Time{}
		} else if goneSince.IsZero() {
			goneSince = time.Now()
		} else if time.Since(goneSince) > 5*time.Second {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// jobAccounting is JOBOBJECT_BASIC_ACCOUNTING_INFORMATION.
type jobAccounting struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

func activeProcesses(job windows.Handle) uint32 {
	if job == 0 {
		return 0
	}
	var info jobAccounting
	if err := windows.QueryInformationJobObject(job, windows.JobObjectBasicAccountingInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil); err != nil {
		return 0
	}
	return info.ActiveProcesses
}

// shellExecuteInfo is SHELLEXECUTEINFOW.
type shellExecuteInfo struct {
	cbSize       uint32
	fMask        uint32
	hwnd         windows.Handle
	lpVerb       *uint16
	lpFile       *uint16
	lpParameters *uint16
	lpDirectory  *uint16
	nShow        int32
	hInstApp     windows.Handle
	lpIDList     uintptr
	lpClass      *uint16
	hkeyClass    windows.Handle
	dwHotKey     uint32
	hIcon        windows.Handle
	hProcess     windows.Handle
}

var procShellExecuteEx = windows.NewLazySystemDLL("shell32.dll").NewProc("ShellExecuteExW")

// shellExecuteRunas starts a program with administrator rights, asking the
// user through UAC, and returns its process.
func shellExecuteRunas(file, args string) (windows.Handle, error) {
	const seeMaskNoCloseProcess = 0x40
	verb, _ := windows.UTF16PtrFromString("runas")
	f, err := windows.UTF16PtrFromString(file)
	if err != nil {
		return 0, err
	}
	var params *uint16
	if args != "" {
		params, _ = windows.UTF16PtrFromString(args)
	}
	info := shellExecuteInfo{
		fMask:        seeMaskNoCloseProcess,
		lpVerb:       verb,
		lpFile:       f,
		lpParameters: params,
		nShow:        windows.SW_SHOWNORMAL,
	}
	info.cbSize = uint32(unsafe.Sizeof(info))
	if ok, _, err := procShellExecuteEx.Call(uintptr(unsafe.Pointer(&info))); ok == 0 {
		return 0, err
	}
	if info.hProcess == 0 {
		return 0, errors.New("the uninstaller started without a process to wait for")
	}
	return info.hProcess, nil
}

// isElevated reports whether the app runs with administrator rights.
func isElevated() bool { return windows.GetCurrentProcessToken().IsElevated() }

// relaunchElevated starts the app again with administrator rights, through
// UAC; it fails when the user declines.
func relaunchElevated(exe string) error {
	verb, _ := windows.UTF16PtrFromString("runas")
	f, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		return err
	}
	args, _ := windows.UTF16PtrFromString("--elevated")
	return windows.ShellExecute(0, verb, f, args, nil, windows.SW_SHOWNORMAL)
}
