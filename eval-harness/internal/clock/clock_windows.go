//go:build windows

package clock

import (
	"syscall"
	"unsafe"
)

var (
	kernel32                  = syscall.NewLazyDLL("kernel32.dll")
	procQueryPerformanceCount = kernel32.NewProc("QueryPerformanceCounter")
	procQueryPerformanceFreq  = kernel32.NewProc("QueryPerformanceFrequency")
	tickNanos                 = func() float64 {
		var freq int64
		procQueryPerformanceFreq.Call(uintptr(unsafe.Pointer(&freq)))
		if freq <= 0 {
			panic("QueryPerformanceFrequency returned no frequency")
		}
		return 1e9 / float64(freq)
	}()
)

// Now reads QueryPerformanceCounter.
func Now() Ticks {
	var value int64
	procQueryPerformanceCount.Call(uintptr(unsafe.Pointer(&value)))
	return Ticks(value)
}

func nanosPerTick() float64 { return tickNanos }

// Source names the clock behind Now.
func Source() string { return "QueryPerformanceCounter" }
