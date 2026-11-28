package cleanup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/continuum-dev/continuum/internal/proc"
	"golang.org/x/sys/unix"
)

var (
	launcherLog = regexp.MustCompile(`^/(?:private/)?(?:tmp/|var/folders/[^/]+/[^/]+/T/)\.tmp[[:alnum:]]+/launcher\.err$`)
	codexTest   = regexp.MustCompile(`^/(?:private/)?(?:tmp/|var/folders/[^/]+/[^/]+/T/)TestCodexExecRequiresStartedTurnForTheTarget[[:alnum:]_-]+/[0-9]{3}$`)
	codexLog    = regexp.MustCompile(`^native-codex-exec-resume-[0-9]+\.log$`)
	daemonTest  = regexp.MustCompile(`^/(?:private/)?tmp/(?:continuum-hook-test|claude-[0-9]+/[^/]+/[^/]+/scratchpad/hook(?:time|test))$`)
)

func checkPrivilege() error {
	if os.Getuid() == 0 || os.Getuid() != os.Geteuid() {
		return errors.New("run cleanup as your regular user")
	}
	return nil
}

func bootID() (string, error) {
	id, err := unix.Sysctl("kern.bootsessionuuid")
	if err != nil {
		return "", fmt.Errorf("read macOS boot session: %w", err)
	}
	if id == "" {
		return "", errors.New("macOS boot session ID is empty")
	}
	return id, nil
}

func snapshot() (inventory, error) {
	entries, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, fmt.Errorf("read macOS process table: %w", err)
	}
	table := make(inventory, len(entries))
	for _, entry := range entries {
		p, e := entry.Proc, entry.Eproc
		// XNU's sys/proc.h defines P_CONTROLT = 0x2 and SZOMB = 5.
		pid := int(p.P_pid)
		name, _, _ := strings.Cut(string(p.P_comm[:]), "\x00")
		table[pid] = process{
			PID: pid, Parent: int(e.Ppid), Group: int(e.Pgid),
			UID: int(e.Ucred.Uid), RealUID: int(e.Pcred.P_ruid),
			Terminal: p.P_flag&0x2 != 0 && e.Tdev != -1, Zombie: p.P_stat == 5,
			Name: name, StartedAt: time.Unix(p.P_starttime.Unix()).UTC(),
		}
	}
	return table, nil
}

func (p process) eligible(now time.Time) bool {
	return p.PID > 1 && p.PID != os.Getpid() && p.Group != unix.Getpgrp() &&
		p.PID == p.Group && p.Parent == 1 && !p.Terminal && !p.Zombie &&
		p.UID == os.Getuid() && p.RealUID == p.UID && p.StartedAt.Unix() > 0 &&
		now.Sub(p.StartedAt) >= time.Hour &&
		(isShell(p.Name) || p.Name == "log" || strings.HasPrefix(p.Name, "continuum"))
}

type descriptor struct {
	kind, name string
	open       bool
}

func (d descriptor) disconnected() bool {
	return !d.open || (d.kind == "CHR" && (d.name == "" || d.name == "(revoked)"))
}

type files struct {
	cwd string
	std [3]descriptor
}

func describe(p process, f files, now time.Time, args []string) string {
	switch {
	case isShell(p.Name) && strings.HasSuffix(f.cwd, "/neo-rsh/crates/neo-launcher") && launcherLog.MatchString(f.std[2].name):
		return launcherReason
	case isShell(p.Name) && codexTest.MatchString(f.cwd) && filepath.Dir(f.std[2].name) == f.cwd &&
		codexLog.MatchString(filepath.Base(f.std[2].name)):
		return codexReason
	case strings.HasPrefix(p.Name, "continuum") && daemonTest.MatchString(f.cwd) &&
		f.std[2].name == filepath.Join(f.cwd, "daemon.stderr.log") && now.Sub(p.StartedAt) >= 12*time.Hour &&
		len(args) >= 2 && args[1] == "daemon":
		return daemonReason
	case p.Name == "log" && f.std[0].name == "(revoked)" && f.std[1].disconnected() && f.std[2].disconnected() &&
		len(args) >= 2 && filepath.Base(args[0]) == "log" && args[1] == "stream":
		return logReason
	default:
		return ""
	}
}

func launchdPIDs() (map[int]bool, error) {
	out, err := proc.Run(5*time.Second, "", "/bin/launchctl", "list")
	if err != nil {
		return nil, fmt.Errorf("list launchd services: %w", err)
	}
	lines := strings.Split(out, "\n")
	header := strings.Fields(lines[0])
	if len(header) != 3 || header[0] != "PID" || header[1] != "Status" || header[2] != "Label" {
		return nil, errors.New("unrecognized launchctl service list")
	}
	result := make(map[int]bool)
	for _, line := range lines[1:] {
		pid, _, _ := strings.Cut(line, "\t")
		if n, err := strconv.Atoi(pid); err == nil && n > 0 {
			result[n] = true
		}
	}
	return result, nil
}

func readFiles(pids []int) (map[int]files, error) {
	list := make([]string, len(pids))
	for i, pid := range pids {
		list[i] = strconv.Itoa(pid)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/sbin/lsof", "-nP", "-w", "-a", "-p",
		strings.Join(list, ","), "-d", "cwd,0,1,2", "-F0pftn")
	out, err := cmd.Output()
	if err != nil {
		// lsof returns 1 without stderr when a selected PID or descriptor
		// vanished. The remaining records are valid in that case.
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 || len(exit.Stderr) != 0 {
			return nil, fmt.Errorf("inspect candidate file descriptors: %w", err)
		}
	}
	return parseFiles(string(out)), nil
}

func parseFiles(out string) map[int]files {
	result := make(map[int]files)
	var pid int
	var fd string
	for _, record := range strings.Split(out, "\x00") {
		record = strings.TrimLeft(record, "\n")
		if record == "" {
			continue
		}
		value := record[1:]
		if record[0] == 'p' {
			pid, _ = strconv.Atoi(value)
			fd = ""
			continue
		}
		if pid == 0 {
			continue
		}
		if record[0] == 'f' {
			fd = value
		}
		f := result[pid]
		if fd == "cwd" && record[0] == 'n' {
			f.cwd = value
		}
		if n, err := strconv.Atoi(fd); err == nil && n >= 0 && n < len(f.std) {
			switch record[0] {
			case 'f':
				f.std[n].open = true
			case 't':
				f.std[n].kind = value
			case 'n':
				f.std[n].name = value
			}
		}
		result[pid] = f
	}
	return result
}

func discover(now time.Time, target *Finding) ([]Finding, map[int]bool, error) {
	findings := []Finding{}
	table, err := snapshot()
	if err != nil {
		return nil, nil, err
	}
	var pids []int
	for pid, p := range table {
		if p.eligible(now) && (target == nil || (pid == target.PID && p.StartedAt.Equal(target.StartedAt))) {
			pids = append(pids, pid)
		}
	}
	if len(pids) == 0 {
		return findings, nil, nil
	}
	slices.Sort(pids)
	registered, err := launchdPIDs()
	if err != nil {
		return nil, nil, err
	}
	for pid := range protectedPIDs() {
		registered[pid] = true
	}
	paths, err := readFiles(pids)
	if err != nil {
		return nil, nil, err
	}
	fresh, err := snapshot()
	if err != nil {
		return nil, nil, err
	}
	for _, pid := range pids {
		p, f := fresh[pid], paths[pid]
		if !p.eligible(now) || p != table[pid] || registered[pid] {
			continue
		}
		var args []string
		if !isShell(p.Name) {
			args = proc.Args(pid)
		}
		reason := describe(p, f, now, args)
		if reason == "" {
			continue
		}
		launcher := reason == launcherReason
		members := fresh.groupMembers(p, registered, launcher)
		if members == 0 || (launcher && !sessionLeader(pid)) {
			continue
		}
		findings = append(findings, Finding{
			PID: pid, Group: p.Group, StartedAt: p.StartedAt, Command: p.Name,
			Reason: reason, Cwd: f.cwd, Stderr: f.std[2].name, Members: members,
		})
	}
	return findings, registered, nil
}

func sessionLeader(pid int) bool {
	sid, err := unix.Getsid(pid)
	return err == nil && sid == pid
}

func terminate(now time.Time, f Finding, registered map[int]bool) (bool, error) {
	prot := protectedPIDs()
	if prot[f.PID] || f.Group == unix.Getpgrp() {
		return false, nil
	}
	merged := make(map[int]bool, len(registered)+len(prot))
	for pid := range registered {
		merged[pid] = true
	}
	for pid := range prot {
		merged[pid] = true
	}
	table, err := snapshot()
	if err != nil {
		return false, err
	}
	p := table[f.PID]
	launcher := f.Reason == launcherReason
	if !p.eligible(now) || !p.StartedAt.Equal(f.StartedAt) || p.Name != f.Command ||
		p.Group != f.Group || table.groupMembers(p, merged, launcher) == 0 {
		return false, nil
	}
	pid := f.PID
	if launcher {
		if !sessionLeader(pid) {
			return false, nil
		}
		pid = -f.Group
	}
	err = unix.Kill(pid, unix.SIGTERM)
	if errors.Is(err, unix.ESRCH) {
		return false, nil
	}
	return err == nil, err
}
