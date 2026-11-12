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
