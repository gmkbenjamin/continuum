//go:build windows

package awake

import (
	"runtime"

	"golang.org/x/sys/windows"
)

const (
	esContinuous     = 0x80000000
	esSystemRequired = 0x00000001
)

var setThreadExecutionState = windows.NewLazySystemDLL("kernel32.dll").NewProc("SetThreadExecutionState")

// The execution state belongs to a thread, so one locked thread owns it.
type winHolder struct{ ch chan uintptr }

func newHolder() holder {
	h := &winHolder{ch: make(chan uintptr)}
	go func() {
		runtime.LockOSThread()
		for flags := range h.ch {
			setThreadExecutionState.Call(flags)
		}
	}()
	return h
}

func (h *winHolder) hold(string) error {
	h.ch <- esContinuous | esSystemRequired
	return nil
}

func (h *winHolder) release() { h.ch <- esContinuous }
