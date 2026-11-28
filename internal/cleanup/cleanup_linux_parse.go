package cleanup

import (
	"encoding/binary"
	"errors"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	linuxLauncherLog = regexp.MustCompile(`^/(?:tmp|var/tmp)/\.tmp[[:alnum:]]+/launcher\.err$`)
	linuxCodexTest   = regexp.MustCompile(`^/(?:tmp|var/tmp)/TestCodexExecRequiresStartedTurnForTheTarget[[:alnum:]_-]+/[0-9]{3}$`)
	linuxCodexLog    = regexp.MustCompile(`^native-codex-exec-resume-[0-9]+\.log$`)
	linuxDaemonTest  = regexp.MustCompile(`^/(?:tmp|var/tmp)/(?:continuum-hook-test|claude-[0-9]+/[^/]+/[^/]+/scratchpad/hook(?:time|test))$`)
)

func parseLinuxBootTime(procStat string) (int64, error) {
	for _, line := range strings.Split(procStat, "\n") {
		if rest, ok := strings.CutPrefix(line, "btime "); ok {
			sec, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
			if err == nil && sec > 0 {
				return sec, nil
			}
		}
	}
	return 0, errors.New("could not read Linux boot time from /proc/stat")
}

// parseLinuxAuxvClkTck reads AT_CLKTCK (type 17) from /proc/self/auxv in host byte order.
func parseLinuxAuxvClkTck(auxv []byte, wordSize int) int64 {
	if wordSize != 4 && wordSize != 8 {
		return 100
	}
	step := wordSize * 2
	for off := 0; off+step <= len(auxv); off += step {
		var tag, val uint64
		if wordSize == 8 {
			tag = binary.NativeEndian.Uint64(auxv[off : off+8])
			val = binary.NativeEndian.Uint64(auxv[off+8 : off+16])
		} else {
			tag = uint64(binary.NativeEndian.Uint32(auxv[off : off+4]))
			val = uint64(binary.NativeEndian.Uint32(auxv[off+4 : off+8]))
		}
		if tag == 0 {
			break
		}
		if tag == 17 && val > 0 && val <= 100000 { // AT_CLKTCK
			return int64(val)
		}
	}
	return 100
}

// parseLinuxProcStat parses /proc/<pid>/stat into a process record plus session ID.
// Format: pid (comm) state ppid pgrp session tty_nr ... starttime(field 22).
func parseLinuxProcStat(statText string, bootSec, clkTck int64) (process, int, bool) {
	openParen := strings.IndexByte(statText, '(')
	closeParen := strings.LastIndexByte(statText, ')')
	if openParen <= 0 || closeParen <= openParen || closeParen+2 > len(statText) {
		return process{}, 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(statText[:openParen]))
	if err != nil || pid <= 0 {
		return process{}, 0, false
	}
	comm := statText[openParen+1 : closeParen]
	fields := strings.Fields(statText[closeParen+1:])
	// After ")": 0=state, 1=ppid, 2=pgrp, 3=session, 4=tty_nr, ..., 19=starttime.
	if len(fields) < 20 {
		return process{}, 0, false
	}
	state := fields[0]
	ppid, err1 := strconv.Atoi(fields[1])
	pgrp, err2 := strconv.Atoi(fields[2])
	sid, err3 := strconv.Atoi(fields[3])
	ttyNr, err4 := strconv.Atoi(fields[4])
	startTicks, err5 := strconv.ParseInt(fields[19], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil || err5 != nil || clkTck <= 0 || startTicks <= 0 {
		return process{}, 0, false
	}
	ns := (startTicks % clkTck) * (int64(time.Second) / clkTck)
	started := time.Unix(bootSec+startTicks/clkTck, ns).UTC()
	return process{
		PID:        pid,
		Parent:     ppid,
		Group:      pgrp,
		Terminal:   ttyNr != 0,
		Zombie:     state == "Z" || state == "X",
		Name:       comm,
		StartedAt:  started,
		StartTicks: startTicks,
	}, sid, true
}

func parseLinuxStatusUIDs(statusText string) (realUID, effUID int, ok bool) {
	for _, line := range strings.Split(statusText, "\n") {
		if rest, found := strings.CutPrefix(line, "Uid:"); found {
			f := strings.Fields(rest)
			if len(f) < 3 {
				return 0, 0, false
			}
			r, err1 := strconv.Atoi(f[0])
			e, err2 := strconv.Atoi(f[1])
			s, err3 := strconv.Atoi(f[2])
			if err1 != nil || err2 != nil || err3 != nil {
				return 0, 0, false
			}
			// Reject setuid/saved-UID mismatch.
			if r != e || r != s {
				return 0, 0, false
			}
			return r, e, true
		}
	}
	return 0, 0, false
}

// isLinuxSystemdServiceCgroup reports whether any cgroup path segment places
// the process inside a systemd service unit (including delegated sub-cgroups,
// excluding the generic user@<uid>.service manager slice), so managed daemons
// like continuum.service are never touched by cleanup.
func isLinuxSystemdServiceCgroup(cgroupText string) bool {
	for _, line := range strings.Split(cgroupText, "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), ":", 3)
		if len(parts) != 3 {
			continue
		}
		for _, seg := range strings.Split(parts[2], "/") {
			if strings.HasSuffix(seg, ".service") && !strings.HasPrefix(seg, "user@") {
				return true
			}
		}
	}
	return false
}

func stripDeletedSuffix(p string) string {
	return strings.TrimSuffix(p, " (deleted)")
}

func describeLinux(p process, cwd, stderr string, now time.Time, args []string) string {
	cwd = stripDeletedSuffix(cwd)
	stderr = stripDeletedSuffix(stderr)
	switch {
	case isShell(p.Name) && strings.HasSuffix(cwd, "/neo-rsh/crates/neo-launcher") && linuxLauncherLog.MatchString(stderr):
		return launcherReason
	case isShell(p.Name) && linuxCodexTest.MatchString(cwd) && path.Dir(stderr) == cwd &&
		linuxCodexLog.MatchString(path.Base(stderr)):
		return codexReason
	case strings.HasPrefix(p.Name, "continuum") && linuxDaemonTest.MatchString(cwd) &&
		stderr == path.Join(cwd, "daemon.stderr.log") && now.Sub(p.StartedAt) >= 12*time.Hour &&
		len(args) >= 2 && args[1] == "daemon":
		return daemonReason
	default:
		return ""
	}
}
