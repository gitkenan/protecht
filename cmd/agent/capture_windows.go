//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// processQueryLimitedInformation is PROCESS_QUERY_LIMITED_INFORMATION.
// It is enough for QueryFullProcessImageName and succeeds for most
// processes without administrator rights. Protected processes still deny it;
// the path is then left empty.
const processQueryLimitedInformation = 0x1000

// maxTitleChars bounds a hostile or huge window title. GetWindowTextW
// truncates to the buffer and still NUL-terminates.
const maxTitleChars = 8192

// maxImageChars is the largest path QueryFullProcessImageNameW will write.
const maxImageChars = 32768

var (
	captureOnce sync.Once
	captureErr  error

	procGetForegroundWindow        *syscall.LazyProc
	procGetWindowTextW             *syscall.LazyProc
	procGetWindowTextLengthW       *syscall.LazyProc
	procGetWindowThreadProcessId   *syscall.LazyProc
	procOpenProcess                *syscall.LazyProc
	procQueryFullProcessImageNameW *syscall.LazyProc
	procCloseHandle                *syscall.LazyProc
)

func captureForeground(now time.Time) (Observation, error) {
	if err := initCapture(); err != nil {
		return Observation{}, err
	}
	hwnd, _, _ := procGetForegroundWindow.Call()
	if hwnd == 0 {
		return Observation{}, fmt.Errorf("no foreground window")
	}
	title := windowTitle(hwnd)
	obs := Observation{Title: title, CapturedAt: now.UTC()}

	var pid uint32
	tid, _, errno := procGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	if tid == 0 {
		return obs, fmt.Errorf("GetWindowThreadProcessId: %w", errno)
	}
	exe := processImage(pid)
	obs.Executable = exe
	obs.Process = processName(exe)
	return obs, nil
}

func initCapture() error {
	captureOnce.Do(func() {
		user32, err := loadSystemDLL("user32.dll")
		if err != nil {
			captureErr = err
			return
		}
		kernel32, err := loadSystemDLL("kernel32.dll")
		if err != nil {
			captureErr = err
			return
		}
		procGetForegroundWindow = user32.NewProc("GetForegroundWindow")
		procGetWindowTextW = user32.NewProc("GetWindowTextW")
		procGetWindowTextLengthW = user32.NewProc("GetWindowTextLengthW")
		procGetWindowThreadProcessId = user32.NewProc("GetWindowThreadProcessId")
		procOpenProcess = kernel32.NewProc("OpenProcess")
		procQueryFullProcessImageNameW = kernel32.NewProc("QueryFullProcessImageNameW")
		procCloseHandle = kernel32.NewProc("CloseHandle")
		for _, p := range []*syscall.LazyProc{
			procGetForegroundWindow,
			procGetWindowTextW,
			procGetWindowTextLengthW,
			procGetWindowThreadProcessId,
			procOpenProcess,
			procQueryFullProcessImageNameW,
			procCloseHandle,
		} {
			if err := p.Find(); err != nil {
				captureErr = fmt.Errorf("%s: %w", p.Name, err)
				return
			}
		}
	})
	return captureErr
}

// loadSystemDLL loads a Windows system DLL by its System32 path. An
// unqualified LoadLibrary name searches the executable directory first, so
// a planted user32.dll next to the agent could steal the bearer token.
func loadSystemDLL(name string) (*syscall.LazyDLL, error) {
	path := filepath.Join(systemRoot(), "System32", name)
	dll := syscall.NewLazyDLL(path)
	if err := dll.Load(); err != nil {
		return nil, fmt.Errorf("load %s: %w", path, err)
	}
	return dll, nil
}

func systemRoot() string {
	if root := os.Getenv("SystemRoot"); root != "" {
		return root
	}
	return `C:\Windows`
}

func windowTitle(hwnd uintptr) string {
	n, _, _ := procGetWindowTextLengthW.Call(hwnd)
	length := int(n)
	if length <= 0 {
		return ""
	}
	if length > maxTitleChars {
		length = maxTitleChars
	}
	buf := make([]uint16, length+1)
	procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return syscall.UTF16ToString(buf)
}

// processImage returns the Win32 path of pid, or "" when the process cannot
// be opened. dwFlags 0 asks for the Win32 path rather than the native NT path.
func processImage(pid uint32) string {
	h, _, _ := procOpenProcess.Call(processQueryLimitedInformation, 0, uintptr(pid))
	if h == 0 {
		return ""
	}
	defer procCloseHandle.Call(h)

	buf := make([]uint16, maxImageChars)
	size := uint32(len(buf))
	r, _, _ := procQueryFullProcessImageNameW.Call(
		h,
		0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)),
	)
	if r == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf)
}
