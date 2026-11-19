//go:build linux

package proc

import (
	"bytes"
	"os"
	"strconv"
)

// Environ returns the environment of one of the user's own processes.
func Environ(pid int) map[string]string {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ")
	if err != nil {
		return nil
	}
	env := map[string]string{}
	for _, kv := range bytes.Split(data, []byte{0}) {
		if eq := bytes.IndexByte(kv, '='); eq > 0 {
			env[string(kv[:eq])] = string(kv[eq+1:])
		}
	}
	return env
}

// Args returns the exact arguments of one of the user's own processes
// (argv[0] first), or nil if they can't be read.
func Args(pid int) []string {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil || len(data) == 0 {
		return nil
	}
	parts := bytes.Split(bytes.TrimRight(data, "\x00"), []byte{0})
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = string(p)
	}
	return out
}
