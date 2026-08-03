package main

import (
	"os"
	"syscall"
	"unsafe"
)

const enableVirtualTerminalProcessing = 0x0004

var kernel32 = syscall.NewLazyDLL("kernel32.dll")

// enableANSI turns on VT escape handling for the console. Windows Terminal does
// this already; legacy conhost does not, and without it the whole session is
// littered with raw escape codes.
func enableANSI() {
	if forceColor {
		return
	}
	getConsoleMode := kernel32.NewProc("GetConsoleMode")
	setConsoleMode := kernel32.NewProc("SetConsoleMode")

	handle := syscall.Handle(os.Stdout.Fd())
	var mode uint32
	if ret, _, _ := getConsoleMode.Call(uintptr(handle), uintptr(unsafe.Pointer(&mode))); ret == 0 {
		useColor = false // not a console: redirected to a file or a pipe
		return
	}
	if mode&enableVirtualTerminalProcessing != 0 {
		return
	}
	if ret, _, _ := setConsoleMode.Call(uintptr(handle), uintptr(mode|enableVirtualTerminalProcessing)); ret == 0 {
		useColor = false
	}
}

type coord struct{ X, Y int16 }
type smallRect struct{ Left, Top, Right, Bottom int16 }

type consoleScreenBufferInfo struct {
	Size              coord
	CursorPosition    coord
	Attributes        uint16
	Window            smallRect
	MaximumWindowSize coord
}

// termWidth reports the console width, falling back to a sane fixed width when
// stdout is not a console.
func termWidth() int {
	var info consoleScreenBufferInfo
	ret, _, _ := kernel32.NewProc("GetConsoleScreenBufferInfo").Call(
		uintptr(syscall.Handle(os.Stdout.Fd())), uintptr(unsafe.Pointer(&info)))
	if ret == 0 {
		return defaultWidth
	}
	if w := int(info.Window.Right - info.Window.Left + 1); w > 20 {
		return w
	}
	return defaultWidth
}
