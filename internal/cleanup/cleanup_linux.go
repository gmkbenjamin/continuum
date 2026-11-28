//go:build linux

package cleanup

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"github.com/continuum-dev/continuum/internal/proc"
	"golang.org/x/sys/unix"
)

func checkPrivilege() error {
	if os.Getuid() == 0 || os.Getuid() != os.Geteuid() {
		return errors.New("run cleanup as your regular user")
	}
	return nil
}

func bootID() (string, error) {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", fmt.Errorf("read Linux boot ID: %w", err)
	}
	id := strings.TrimSpace(string(data))
	if id == "" {
		return "", errors.New("Linux boot ID is empty")
	}
	return id, nil
}

func linuxClockTick() int64 {
	data, err := os.ReadFile("/proc/self/auxv")
	if err != nil {
		return 100
	}
	return parseLinuxAuxvClkTck(data, int(unsafe.Sizeof(uintptr(0))))
}

func snapshot() (inventory, error) {
	statData, err := os.ReadFile("/proc/stat")
	if err != nil {
		return nil, fmt.Errorf("read /proc/stat: %w", err)
	}
	bootSec, err := parseLinuxBootTime(string(statData))
	if err != nil {
		return nil, err
	}
	clkTck := linuxClockTick()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("read /proc: %w", err)
	}
	table := make(inventory, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		rawStat, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		p, _, ok := parseLinuxProcStat(string(rawStat), bootSec, clkTck)
		if !ok {
			continue
		}
		rawStatus, err := os.ReadFile("/proc/" + e.Name() + "/status")
		if err != nil {
			continue
		}
		realUID, effUID, ok := parseLinuxStatusUIDs(string(rawStatus))
		if !ok {
			continue
		}
		p.RealUID, p.UID = realUID, effUID
		table[pid] = p
	}
	return table, nil
}

// orphanParents returns PIDs that legitimately adopt orphaned user processes:
// PID 1 and the current user's `systemd --user` subreaper if running.
func orphanParents(table inventory) map[int]bool {
	parents := map[int]bool{1: true}
	uid := os.Getuid()
	for pid, p := range table {
		if pid <= 1 || p.UID != uid || p.RealUID != uid || p.Name != "systemd" {
			continue
		}
		if slices.Contains(proc.Args(pid), "--user") {
			parents[pid] = true
		}
	}
	return parents
}

func (p process) eligible(now time.Time, orphans map[int]bool) bool {
	return p.PID > 1 && p.PID != os.Getpid() && p.Group != unix.Getpgrp() &&
		p.PID == p.Group && orphans[p.Parent] && !p.Terminal && !p.Zombie &&
		p.UID == os.Getuid() && p.RealUID == p.UID && p.StartTicks > 0 &&
		now.Sub(p.StartedAt) >= time.Hour &&
		(isShell(p.Name) || strings.HasPrefix(p.Name, "continuum"))
}

func sameLinuxProcess(a, b process) bool {
	return a.PID == b.PID && a.Parent == b.Parent && a.Group == b.Group &&
		a.UID == b.UID && a.RealUID == b.RealUID &&
		a.Terminal == b.Terminal && a.Zombie == b.Zombie &&
		a.Name == b.Name && a.StartTicks == b.StartTicks
}

func sessionLeader(pid int) bool {
	sid, err := unix.Getsid(pid)
	return err == nil && sid == pid
}

func registeredPIDs(table inventory) (map[int]bool, error) {
	out := protectedPIDs()
	uid := os.Getuid()
	for pid, p := range table {
		if p.UID != uid || p.RealUID != uid {
			continue
		}
		data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cgroup")
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			// Fail closed if a user-owned process's cgroup cannot be verified.
			out[pid] = true
			continue
		}
		if isLinuxSystemdServiceCgroup(string(data)) {
			out[pid] = true
		}
	}
	return out, nil
}

func discover(now time.Time, target *Finding) ([]Finding, map[int]bool, error) {
	findings := []Finding{}
	table, err := snapshot()
	if err != nil {
		return nil, nil, err
	}
	orphans := orphanParents(table)
	var pids []int
	for pid, p := range table {
		if p.eligible(now, orphans) && (target == nil || (pid == target.PID && sameStart(*target, p))) {
			pids = append(pids, pid)
		}
	}
	if len(pids) == 0 {
		return findings, nil, nil
	}
	slices.Sort(pids)
	registered, err := registeredPIDs(table)
	if err != nil {
		return nil, nil, err
	}

	type procFiles struct{ cwd, stderr string }
	paths := make(map[int]procFiles, len(pids))
	for _, pid := range pids {
		base := "/proc/" + strconv.Itoa(pid)
		cwd, _ := os.Readlink(base + "/cwd")
		stderr, _ := os.Readlink(base + "/fd/2")
		paths[pid] = procFiles{cwd: cwd, stderr: stderr}
	}
	fresh, err := snapshot()
	if err != nil {
		return nil, nil, err
	}
	freshOrphans := orphanParents(fresh)
	for _, pid := range pids {
		p, f := fresh[pid], paths[pid]
		if !p.eligible(now, freshOrphans) || !sameLinuxProcess(p, table[pid]) || registered[pid] {
			continue
		}
		var args []string
		if !isShell(p.Name) {
			args = proc.Args(pid)
		}
		reason := describeLinux(p, f.cwd, f.stderr, now, args)
		if reason == "" {
			continue
		}
		launcher := reason == launcherReason
		members := fresh.groupMembers(p, registered, launcher)
		if members == 0 || (launcher && !sessionLeader(pid)) {
			continue
		}
		findings = append(findings, Finding{
			PID: pid, Group: p.Group, StartedAt: p.StartedAt, StartTicks: p.StartTicks, Command: p.Name,
			Reason: reason, Cwd: stripDeletedSuffix(f.cwd), Stderr: stripDeletedSuffix(f.stderr), Members: members,
		})
	}
	return findings, registered, nil
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
	orphans := orphanParents(table)
	p := table[f.PID]
	launcher := f.Reason == launcherReason
	if !p.eligible(now, orphans) || !sameStart(f, p) || p.Name != f.Command ||
		p.Group != f.Group || table.groupMembers(p, merged, launcher) == 0 {
		return false, nil
	}
	if launcher {
		if !sessionLeader(f.PID) {
			return false, nil
		}
		err = unix.Kill(-f.Group, unix.SIGTERM)
		if errors.Is(err, unix.ESRCH) {
			return false, nil
		}
		return err == nil, err
	}
	// Prefer pidfd_open + re-check + pidfd_send_signal on Linux 5.1+ to close
	// the singleton PID-reuse TOCTOU window.
	if pidfd, err := unix.PidfdOpen(f.PID, 0); err == nil {
		defer unix.Close(pidfd)
		rawStat, rerr := os.ReadFile("/proc/" + strconv.Itoa(f.PID) + "/stat")
		if rerr != nil {
			return false, nil
		}
		cur, _, ok := parseLinuxProcStat(string(rawStat), 0, linuxClockTick())
		if !ok || !sameStart(f, cur) || cur.Name != f.Command || cur.Group != f.Group {
			return false, nil
		}
		serr := unix.PidfdSendSignal(pidfd, unix.SIGTERM, nil, 0)
		if errors.Is(serr, unix.ESRCH) {
			return false, nil
		}
		if !errors.Is(serr, unix.ENOSYS) {
			return serr == nil, serr
		}
	} else if !errors.Is(err, unix.ENOSYS) && !errors.Is(err, unix.EPERM) {
		if errors.Is(err, unix.ESRCH) {
			return false, nil
		}
	}
	err = unix.Kill(f.PID, unix.SIGTERM)
	if errors.Is(err, unix.ESRCH) {
		return false, nil
	}
	return err == nil, err
}
