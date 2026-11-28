//go:build windows

package cleanup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unsafe"

	"github.com/continuum-dev/continuum/internal/proc"
	"golang.org/x/sys/windows"
)

func checkPrivilege() error {
	tok := windows.GetCurrentProcessToken()
	if tok.IsElevated() {
		return errors.New("run cleanup as your regular non-elevated user")
	}
	if u, err := tok.GetTokenUser(); err == nil && u != nil && u.User.Sid != nil {
		if sysSID, serr := windows.CreateWellKnownSid(windows.WinLocalSystemSid); serr == nil && u.User.Sid.Equals(sysSID) {
			return errors.New("run cleanup as your regular user")
		}
	}
	return nil
}

type systemTimeOfDayInfo struct {
	BootTime      int64
	CurrentTime   int64
	TimeZoneBias  int64
	TimeZoneId    uint32
	Reserved      uint32
	BootTimeBias  uint64
	SleepTimeBias uint64
}

func bootID() (string, error) {
	var info systemTimeOfDayInfo
	var retLen uint32
	err := windows.NtQuerySystemInformation(
		windows.SystemTimeOfDayInformation,
		unsafe.Pointer(&info),
		uint32(unsafe.Sizeof(info)),
		&retLen,
	)
	if err != nil {
		return "", fmt.Errorf("read Windows boot time: %w", err)
	}
	if info.BootTime <= 0 {
		return "", errors.New("Windows boot time is empty")
	}
	return fmt.Sprintf("winboot-%x", info.BootTime), nil
}

func currentUserSID() (*windows.SID, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	return u.User.Sid, nil
}

func processOwnedBy(h windows.Handle, want *windows.SID) bool {
	if want == nil {
		return false
	}
	var tok windows.Token
	if err := windows.OpenProcessToken(h, windows.TOKEN_QUERY, &tok); err != nil {
		return false
	}
	defer tok.Close()
	u, err := tok.GetTokenUser()
	if err != nil || u == nil || u.User.Sid == nil {
		return false
	}
	return u.User.Sid.Equals(want)
}

func processStartAndName(h windows.Handle) (time.Time, string, bool) {
	var create, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &create, &exit, &kernel, &user); err != nil {
		return time.Time{}, "", false
	}
	started := time.Unix(0, create.Nanoseconds()).UTC()
	var buf [windows.MAX_LONG_PATH]uint16
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return started, "", true
	}
	return started, filepath.Base(windows.UTF16ToString(buf[:size])), true
}

func snapshot() (inventory, error) {
	selfSID, err := currentUserSID()
	if err != nil {
		return nil, fmt.Errorf("read Windows user SID: %w", err)
	}
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, fmt.Errorf("read Windows process table: %w", err)
	}
	defer windows.CloseHandle(snap)
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	table := make(inventory)
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		pid := int(e.ProcessID)
		if pid <= 0 {
			continue
		}
		p := process{
			PID:    pid,
			Parent: int(e.ParentProcessID),
			Name:   windows.UTF16ToString(e.ExeFile[:]),
		}
		h, openErr := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, e.ProcessID)
		if openErr == nil {
			var code uint32
			if windows.GetExitCodeProcess(h, &code) == nil && code != 259 {
				p.Zombie = true
			}
			if started, imgName, ok := processStartAndName(h); ok {
				p.StartedAt = started
				if imgName != "" {
					p.Name = imgName
				}
			}
			if processOwnedBy(h, selfSID) {
				p.UID = 1
				p.RealUID = 1
			}
			_ = windows.CloseHandle(h)
		}
		table[pid] = p
	}
	return table, nil
}

func (p process) eligibleWindows(now time.Time, table inventory) bool {
	if p.PID <= 0 || p.PID == os.Getpid() || p.Zombie || p.UID != 1 || p.RealUID != 1 {
		return false
	}
	if p.StartedAt.Unix() <= 0 || now.Sub(p.StartedAt) < time.Hour {
		return false
	}
	// On Windows an orphaned process has a parent PID that is no longer alive
	// (or whose start time is newer than the child due to PID reuse).
	if parent, alive := table[p.Parent]; alive && !parent.Zombie && !parent.StartedAt.After(p.StartedAt) {
		return false
	}
	return isWindowsCleanupCandidateName(p.Name)
}

func hasLiveChildren(table inventory, pid int, leaderStart time.Time) bool {
	for _, p := range table {
		if p.PID == pid || p.Zombie || p.Parent != pid {
			continue
		}
		if !p.StartedAt.IsZero() && !leaderStart.IsZero() && p.StartedAt.Before(leaderStart) {
			continue
		}
		return true
	}
	return false
}

func discover(now time.Time, target *Finding) ([]Finding, map[int]bool, error) {
	findings := []Finding{}
	table, err := snapshot()
	if err != nil {
		return nil, nil, err
	}
	registered := protectedPIDs()
	var pids []int
	for pid, p := range table {
		if registered[pid] {
			continue
		}
		if p.eligibleWindows(now, table) && (target == nil || (pid == target.PID && p.StartedAt.Equal(target.StartedAt))) {
			pids = append(pids, pid)
		}
	}
	if len(pids) == 0 {
		return findings, registered, nil
	}
	slices.Sort(pids)

	type winMeta struct {
		cwd  string
		args []string
	}
	metas := make(map[int]winMeta, len(pids))
	for _, pid := range pids {
		metas[pid] = winMeta{cwd: proc.Cwd(pid), args: proc.Args(pid)}
	}
	// Re-snapshot after reading per-PID CWD/Args so metadata from a reused PID
	// can never be attributed to a different process.
	fresh, err := snapshot()
	if err != nil {
		return nil, nil, err
	}
	for _, pid := range pids {
		p := fresh[pid]
		if p != table[pid] || !p.eligibleWindows(now, fresh) || registered[pid] {
			continue
		}
		if hasLiveChildren(fresh, pid, p.StartedAt) {
			continue
		}
		m := metas[pid]
		reason := describeWindows(p.Name, m.cwd, now, p.StartedAt, m.args)
		if reason == "" {
			continue
		}
		findings = append(findings, Finding{
			PID: pid, Group: 0, StartedAt: p.StartedAt, Command: p.Name,
			Reason: reason, Cwd: m.cwd, Members: 1,
		})
	}
	return findings, registered, nil
}

func terminate(now time.Time, f Finding, registered map[int]bool) (bool, error) {
	if protectedPIDs()[f.PID] || registered[f.PID] {
		return false, nil
	}
	selfSID, err := currentUserSID()
	if err != nil {
		return false, err
	}
	table, err := snapshot()
	if err != nil {
		return false, err
	}
	p, exists := table[f.PID]
	if !exists || !p.eligibleWindows(now, table) || !p.StartedAt.Equal(f.StartedAt) || hasLiveChildren(table, f.PID, p.StartedAt) {
		return false, nil
	}
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(f.PID))
	if err != nil {
		return false, nil
	}
	defer windows.CloseHandle(h)
	// Re-verify process identity, ownership, and image name on the open handle
	// (fail closed if QueryFullProcessImageName returns empty).
	if !processOwnedBy(h, selfSID) {
		return false, nil
	}
	started, imgName, ok := processStartAndName(h)
	if !ok || imgName == "" || !started.Equal(f.StartedAt) || !strings.EqualFold(imgName, f.Command) {
		return false, nil
	}
	if now.Sub(started) < time.Hour {
		return false, nil
	}
	if err := windows.TerminateProcess(h, 1); err != nil {
		var code uint32
		if windows.GetExitCodeProcess(h, &code) == nil && code != 259 {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
