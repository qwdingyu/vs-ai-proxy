//go:build windows

package main

import (
	"math"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	consoleFontFaceNameLen = 32
	minConsoleFontHeight   = 10
	consoleWidthMultiplier = 1.10
)

// consoleFontCandidates 优先使用 Windows 中文字体，避免中文日志在双击 exe 的控制台中显示为方块。
// Consolas 是兜底项；字体设置失败时保持系统默认，不影响服务启动。
var consoleFontCandidates = []string{"Microsoft YaHei UI", "Microsoft YaHei Mono", "Consolas"}

var (
	kernel32                       = windows.NewLazySystemDLL("kernel32.dll")
	procGetCurrentConsoleFontEx    = kernel32.NewProc("GetCurrentConsoleFontEx")
	procSetCurrentConsoleFontEx    = kernel32.NewProc("SetCurrentConsoleFontEx")
	procGetConsoleScreenBufferInfo = kernel32.NewProc("GetConsoleScreenBufferInfo")
	procSetConsoleScreenBufferSize = kernel32.NewProc("SetConsoleScreenBufferSize")
	procSetConsoleWindowInfo       = kernel32.NewProc("SetConsoleWindowInfo")
)

type consoleCoord struct {
	X int16
	Y int16
}

type consoleSmallRect struct {
	Left   int16
	Top    int16
	Right  int16
	Bottom int16
}

type consoleScreenBufferInfo struct {
	Size              consoleCoord
	CursorPosition    consoleCoord
	Attributes        uint16
	Window            consoleSmallRect
	MaximumWindowSize consoleCoord
}

type consoleFontInfoEx struct {
	Size       uint32
	FontIndex  uint32
	FontSize   consoleCoord
	FontFamily uint32
	FontWeight uint32
	FaceName   [consoleFontFaceNameLen]uint16
}

func configureConsoleWindow() {
	handle, err := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE)
	if err != nil || handle == windows.InvalidHandle {
		return
	}

	configureConsoleFont(handle)
	widenConsoleWindow(handle, consoleWidthMultiplier)
}

func configureConsoleFont(handle windows.Handle) {
	info := consoleFontInfoEx{Size: uint32(unsafe.Sizeof(consoleFontInfoEx{}))}
	if ok := callBool(procGetCurrentConsoleFontEx, uintptr(handle), 0, uintptr(unsafe.Pointer(&info))); !ok {
		return
	}

	// 字号按用户要求缩小 2 个高度单位；下限保护避免远程/系统默认小字体被压到不可读。
	if info.FontSize.Y > minConsoleFontHeight+2 {
		info.FontSize.Y -= 2
	} else if info.FontSize.Y > minConsoleFontHeight {
		info.FontSize.Y = minConsoleFontHeight
	}
	_ = callBool(procSetCurrentConsoleFontEx, uintptr(handle), 0, uintptr(unsafe.Pointer(&info)))
	for _, faceName := range consoleFontCandidates {
		setConsoleFaceName(&info, faceName)
		if callBool(procSetCurrentConsoleFontEx, uintptr(handle), 0, uintptr(unsafe.Pointer(&info))) {
			return
		}
	}
}

func widenConsoleWindow(handle windows.Handle, multiplier float64) {
	info := consoleScreenBufferInfo{}
	if ok := callBool(procGetConsoleScreenBufferInfo, uintptr(handle), uintptr(unsafe.Pointer(&info))); !ok {
		return
	}

	currentWidth := int(info.Window.Right - info.Window.Left + 1)
	if currentWidth <= 0 {
		return
	}
	targetWidth := int(math.Ceil(float64(currentWidth) * multiplier))
	if maxWidth := int(info.MaximumWindowSize.X); maxWidth > 0 && targetWidth > maxWidth {
		targetWidth = maxWidth
	}
	if targetWidth <= currentWidth {
		return
	}

	// Windows 要求窗口矩形不能超过 screen buffer；先扩大 buffer，再调整可见窗口。
	bufferWidth := info.Size.X
	if int(bufferWidth) < targetWidth {
		bufferWidth = int16(targetWidth)
	}
	bufferSize := consoleCoord{X: bufferWidth, Y: info.Size.Y}
	_, _, _ = procSetConsoleScreenBufferSize.Call(uintptr(handle), coordToUintptr(bufferSize))

	nextWindow := info.Window
	nextWindow.Right = nextWindow.Left + int16(targetWidth) - 1
	_ = callBool(procSetConsoleWindowInfo, uintptr(handle), 1, uintptr(unsafe.Pointer(&nextWindow)))
}

func setConsoleFaceName(info *consoleFontInfoEx, name string) {
	encoded := windows.StringToUTF16(name)
	for i := range info.FaceName {
		info.FaceName[i] = 0
	}
	for i, value := range encoded {
		if i >= len(info.FaceName) {
			break
		}
		info.FaceName[i] = value
	}
}

func coordToUintptr(coord consoleCoord) uintptr {
	return uintptr(uint32(uint16(coord.X)) | uint32(uint16(coord.Y))<<16)
}

func callBool(proc *windows.LazyProc, args ...uintptr) bool {
	ret, _, _ := proc.Call(args...)
	return ret != 0
}
