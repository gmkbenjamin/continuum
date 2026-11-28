//go:build windows

package proc

import (
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Pure-uintptr mirror structs of Windows NT process structures so remote
// process virtual addresses are NEVER stored in Go pointer-typed fields
// (avoiding Go GC invalidptr crashes when scanning remote addresses).

const rtlUserProcParamsNormalized = 1

type remotePBI struct {
	ExitStatus                   int32
	_                            uint32
	PebBaseAddress               uintptr
	AffinityMask                 uintptr
	BasePriority                 int32
	_                            uint32
	UniqueProcessId              uintptr
	InheritedFromUniqueProcessId uintptr
}

type remotePEBHeader struct {
	Reserved1         [2]byte
	BeingDebugged     byte
	BitField          byte
	Reserved3         uintptr
	ImageBaseAddress  uintptr
	Ldr               uintptr
	ProcessParameters uintptr
}

type remoteUnicodeString struct {
	Length        uint16
	MaximumLength uint16
	Buffer        uintptr
}

type remoteCurDir struct {
	DosPath remoteUnicodeString
	Handle  uintptr
}

// remoteProcessParamsCore covers RTL_USER_PROCESS_PARAMETERS from offset 0
// through Environment (identical across all Windows versions since XP, so it
// never over-reads past the struct end on Windows 10 1809 / Server 2019).
type remoteProcessParamsCore struct {
	MaximumLength    uint32
	Length           uint32
	Flags            uint32
	DebugFlags       uint32
	ConsoleHandle    uintptr
	ConsoleFlags     uint32
	StandardInput    uintptr
	StandardOutput   uintptr
	StandardError    uintptr
	CurrentDirectory remoteCurDir
	DllPath          remoteUnicodeString
	ImagePathName    remoteUnicodeString
	CommandLine      remoteUnicodeString
	Environment      uintptr
}

// Compile-time layout sanity checks against golang.org/x/sys/windows definitions.
var (
	_ [unsafe.Offsetof(windows.PEB{}.ProcessParameters)]byte = [unsafe.Offsetof(remotePEBHeader{}.ProcessParameters)]byte{}
	_ [unsafe.Offsetof(windows.RTL_USER_PROCESS_PARAMETERS{}.CurrentDirectory)]byte = [unsafe.Offsetof(remoteProcessParamsCore{}.CurrentDirectory)]byte{}
	_ [unsafe.Offsetof(windows.RTL_USER_PROCESS_PARAMETERS{}.CommandLine)]byte      = [unsafe.Offsetof(remoteProcessParamsCore{}.CommandLine)]byte{}
	_ [unsafe.Offsetof(windows.RTL_USER_PROCESS_PARAMETERS{}.Environment)]byte      = [unsafe.Offsetof(remoteProcessParamsCore{}.Environment)]byte{}
	_ [unsafe.Offsetof(windows.PROCESS_BASIC_INFORMATION{}.PebBaseAddress)]byte     = [unsafe.Offsetof(remotePBI{}.PebBaseAddress)]byte{}
)

func resolveRemoteAddr(base uintptr, normalized bool, raw uintptr) uintptr {
	if raw == 0 {
		return 0
	}
	if !normalized {
		return base + raw
	}
	return raw
}

func readRemoteUnicodeString(h windows.Handle, paramsAddr uintptr, normalized bool, u remoteUnicodeString) (string, bool) {
	if u.Length == 0 || u.Buffer == 0 || u.Length%2 != 0 {
		return "", false
	}
	addr := resolveRemoteAddr(paramsAddr, normalized, u.Buffer)
	buf := make([]uint16, u.Length/2)
	var read uintptr
	err := windows.ReadProcessMemory(
		h,
		addr,
		(*byte)(unsafe.Pointer(&buf[0])),
		uintptr(u.Length),
		&read,
	)
	if err != nil || read < 2 {
		return "", false
	}
	return windows.UTF16ToString(buf[:read/2]), true
}

func readRemoteEnvBlock(h windows.Handle, base uintptr, sizeHint uintptr) map[string]string {
	if base == 0 {
		return nil
	}
	const maxBytes = 256 * 1024
	const chunkBytes = 8 * 1024
	limit := sizeHint
	if limit == 0 || limit > maxBytes {
		limit = maxBytes
	}
	var all []uint16
	addr := base
	for uintptr(len(all)*2) < limit {
		want := uintptr(chunkBytes)
		if rem := limit - uintptr(len(all)*2); rem < want {
			want = rem
		}
		if want < 2 {
			break
		}
		chunk := make([]uint16, want/2)
		var read uintptr
		err := windows.ReadProcessMemory(h, addr, (*byte)(unsafe.Pointer(&chunk[0])), uintptr(len(chunk)*2), &read)
		if read < 2 {
			break
		}
		n := int(read / 2)
		all = append(all, chunk[:n]...)
		addr += uintptr(n * 2)
		// Stop once we have seen a double-NUL terminator.
		for i := max(0, len(all)-n-1); i+1 < len(all); i++ {
			if all[i] == 0 && all[i+1] == 0 {
				return parseWinEnvBlock(all[:i+2])
			}
		}
		if err != nil {
			break
		}
	}
	return parseWinEnvBlock(all)
}

func sameWow64(target windows.Handle) bool {
	var selfW, targetW bool
	if err := windows.IsWow64Process(windows.CurrentProcess(), &selfW); err != nil {
		return false
	}
	if err := windows.IsWow64Process(target, &targetW); err != nil {
		return false
	}
	return selfW == targetW
}

func ntProcessParams(pid int, wantEnv bool) (cwd, cmdLine string, env map[string]string, ok bool) {
	if pid <= 0 {
		return "", "", nil, false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ, false, uint32(pid))
	if err != nil {
		return "", "", nil, false
	}
	defer windows.CloseHandle(h)
	if !sameWow64(h) {
		return "", "", nil, false
	}
	var pbi remotePBI
	var retLen uint32
	if err := windows.NtQueryInformationProcess(
		h,
		windows.ProcessBasicInformation,
		unsafe.Pointer(&pbi),
		uint32(unsafe.Sizeof(pbi)),
		&retLen,
	); err != nil || pbi.PebBaseAddress == 0 {
		return "", "", nil, false
	}
	var peb remotePEBHeader
	if err := windows.ReadProcessMemory(
		h,
		pbi.PebBaseAddress,
		(*byte)(unsafe.Pointer(&peb)),
		unsafe.Sizeof(peb),
		nil,
	); err != nil || peb.ProcessParameters == 0 {
		return "", "", nil, false
	}
	var params remoteProcessParamsCore
	if err := windows.ReadProcessMemory(
		h,
		peb.ProcessParameters,
		(*byte)(unsafe.Pointer(&params)),
		unsafe.Sizeof(params),
		nil,
	); err != nil {
		return "", "", nil, false
	}
	normalized := params.Flags&rtlUserProcParamsNormalized != 0
	if dir, got := readRemoteUnicodeString(h, peb.ProcessParameters, normalized, params.CurrentDirectory.DosPath); got {
		// Trim trailing separator unless it is a drive root like C:\.
		if len(dir) > 3 && strings.HasSuffix(dir, `\`) {
			dir = strings.TrimRight(dir, `\`)
		}
		cwd = dir
	}
	if cmd, got := readRemoteUnicodeString(h, peb.ProcessParameters, normalized, params.CommandLine); got {
		cmdLine = cmd
	}
	if wantEnv && params.Environment != 0 {
		var envSize uintptr
		envSizeOff := unsafe.Offsetof(windows.RTL_USER_PROCESS_PARAMETERS{}.EnvironmentSize)
		if uintptr(params.Length) >= envSizeOff+unsafe.Sizeof(envSize) {
			_ = windows.ReadProcessMemory(
				h,
				peb.ProcessParameters+envSizeOff,
				(*byte)(unsafe.Pointer(&envSize)),
				unsafe.Sizeof(envSize),
				nil,
			)
		}
		envAddr := resolveRemoteAddr(peb.ProcessParameters, normalized, params.Environment)
		env = readRemoteEnvBlock(h, envAddr, envSize)
	}
	return cwd, cmdLine, env, true
}

func ntCommandLineFallback(pid int) string {
	if pid <= 0 {
		return ""
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	bufSize := uint32(4096)
	for attempt := 0; attempt < 2; attempt++ {
		buf := make([]byte, bufSize)
		var retLen uint32
		err = windows.NtQueryInformationProcess(
			h,
			windows.ProcessCommandLineInformation,
			unsafe.Pointer(&buf[0]),
			bufSize,
			&retLen,
		)
		if err == nil {
			u := (*windows.NTUnicodeString)(unsafe.Pointer(&buf[0]))
			return u.String()
		}
		if retLen > bufSize && retLen <= 64*1024 {
			bufSize = retLen
			continue
		}
		break
	}
	return ""
}

// Environ returns the environment of one of the user's own processes on Windows.
func Environ(pid int) map[string]string {
	if pid == os.Getpid() {
		out := map[string]string{}
		for _, kv := range os.Environ() {
			if eq := strings.IndexByte(kv, '='); eq > 0 {
				out[kv[:eq]] = kv[eq+1:]
			}
		}
		return out
	}
	_, _, env, _ := ntProcessParams(pid, true)
	return env
}

// Args returns the parsed command-line arguments of pid on Windows.
func Args(pid int) []string {
	if pid == os.Getpid() {
		return append([]string(nil), os.Args...)
	}
	if _, cmdLine, _, ok := ntProcessParams(pid, false); ok && cmdLine != "" {
		return splitWinCommandLine(cmdLine)
	}
	if cmdLine := ntCommandLineFallback(pid); cmdLine != "" {
		return splitWinCommandLine(cmdLine)
	}
	return nil
}

func winCwd(pid int) string {
	if pid == os.Getpid() {
		dir, _ := os.Getwd()
		return dir
	}
	cwd, _, _, _ := ntProcessParams(pid, false)
	return cwd
}
