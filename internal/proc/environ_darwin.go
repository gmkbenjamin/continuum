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
	buf, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil || len(buf) < 4 {
		return nil
	}
	argc := int(binary.LittleEndian.Uint32(buf[:4]))
	rest := buf[4:]
	// Skip the exec path and the NUL padding after it.
	i := bytes.IndexByte(rest, 0)
	if i < 0 {
		return nil
	}
	rest = rest[i:]
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}
	parts := bytes.Split(rest, []byte{0})
	if len(parts) <= argc {
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
