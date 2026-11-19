//go:build darwin

package proc

import (
	"bytes"
	"encoding/binary"

	"golang.org/x/sys/unix"
)

// Environ returns the environment of one of the user's own processes, read
// from the kernel (kern.procargs2): exec path, argv, then the environment,
// all NUL-separated. Nil if it can't be read.
func Environ(pid int) map[string]string {
	parts, argc := procargs(pid)
	if parts == nil || len(parts) <= argc {
		return nil
	}
	env := map[string]string{}
	for _, kv := range parts[argc:] {
		if len(kv) == 0 {
			break
		}
		if eq := bytes.IndexByte(kv, '='); eq > 0 {
			env[string(kv[:eq])] = string(kv[eq+1:])
		}
	}
	return env
}

// Args returns the exact arguments of one of the user's own processes
// (argv[0] first), or nil if they can't be read.
func Args(pid int) []string {
	parts, argc := procargs(pid)
	if parts == nil || argc <= 0 || len(parts) < argc {
		return nil
	}
	out := make([]string, argc)
	for i := range out {
		out[i] = string(parts[i])
	}
	return out
}

// procargs reads kern.procargs2: argc, the exec path, NUL padding, then the
// arguments and the environment, all NUL-separated.
func procargs(pid int) ([][]byte, int) {
	buf, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil || len(buf) < 4 {
		return nil, 0
	}
	argc := int(binary.LittleEndian.Uint32(buf[:4]))
	rest := buf[4:]
	// Skip the exec path and the NUL padding after it.
	i := bytes.IndexByte(rest, 0)
	if i < 0 {
		return nil, 0
	}
	rest = rest[i:]
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}
	return bytes.Split(rest, []byte{0}), argc
}
