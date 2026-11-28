package cleanup

import (
	"encoding/binary"
	"os"
	"testing"
	"time"
)

func TestGroupMembershipRejectsUnrelatedProcesses(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	leader := process{PID: 10, Group: 10, Parent: 1, UID: 501, RealUID: 501, Name: "sh", StartedAt: now}
	child := process{PID: 12, Group: 10, Parent: 10, UID: 501, RealUID: 501, Name: "sh", StartedAt: now.Add(time.Second)}
	conventional := inventory{10: leader, 12: child}
	if got := conventional.groupMembers(leader, nil, true); got != 2 {
		t.Fatalf("recognized shell group has %d members, want 2", got)
	}
	if got := conventional.groupMembers(leader, map[int]bool{12: true}, true); got != 0 {
		t.Fatal("registered child accepted")
	}
	if got := conventional.groupMembers(leader, nil, false); got != 0 {
		t.Fatal("supposed singleton has a live child")
	}
	// Reject an older process sharing the same process group.
	older := child
	older.StartedAt = now.Add(-time.Minute)
	if got := (inventory{10: leader, 12: older}).groupMembers(leader, nil, true); got != 0 {
		t.Fatal("older member sharing group accepted")
	}
	child.Group = 12
	conventional[12] = child
	if got := conventional.groupMembers(leader, nil, true); got != 0 {
		t.Fatal("group with a descendant outside the group accepted")
	}
}

func TestProtectedPIDsIncludesSelf(t *testing.T) {
	prot := protectedPIDs()
	if !prot[os.Getpid()] {
		t.Fatal("protectedPIDs must always include current PID")
	}
}

func TestTargetStillRunningHandlesPIDReuseAndWindowsSingletons(t *testing.T) {
	t0 := time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)
	pt := pendingTarget{
		finding:   Finding{PID: 42, Group: 0, StartedAt: t0, Reason: daemonReason},
		groupKill: false,
	}
	// Same PID + same StartedAt -> still running.
	if !targetStillRunning(inventory{42: {PID: 42, StartedAt: t0}}, pt) {
		t.Fatal("expected matching singleton to be still running")
	}
	// Same PID reused with newer StartedAt -> terminated!
	if targetStillRunning(inventory{42: {PID: 42, StartedAt: t0.Add(time.Second)}}, pt) {
		t.Fatal("reused PID should not count as still running")
	}
	// Group target: unrelated older process sharing PGID does not keep it pending.
	gpt := pendingTarget{
		finding:   Finding{PID: 100, Group: 100, StartedAt: t0, Reason: launcherReason},
		groupKill: true,
	}
	if targetStillRunning(inventory{99: {PID: 99, Group: 100, StartedAt: t0.Add(-time.Hour)}}, gpt) {
		t.Fatal("older unrelated process sharing PGID should not keep group pending")
	}
	if !targetStillRunning(inventory{101: {PID: 101, Group: 100, StartedAt: t0.Add(time.Second)}}, gpt) {
		t.Fatal("living younger group member should keep group pending")
	}
}

func TestSelectChecksEveryReviewedPID(t *testing.T) {
	scan := Scan{ID: "review", Findings: []Finding{{PID: 123}, {PID: 456}}}
	if got, err := scan.Select([]int{123, 999}, false); err == nil || len(got) != 0 {
		t.Fatalf("partial selection after unknown PID: %v, %v", got, err)
	}
	if got, err := scan.Select([]int{456, 456}, false); err != nil || len(got) != 1 || got[0].PID != 456 {
		t.Fatalf("duplicate selections: %v, %v", got, err)
	}
}

func TestLinuxProcParsersAndClassifier(t *testing.T) {
	boot, err := parseLinuxBootTime("cpu  1 2 3\nbtime 1700000000\nprocesses 50\n")
	if err != nil || boot != 1700000000 {
		t.Fatalf("boot = %d, err = %v", boot, err)
	}
	var auxv [32]byte
	binary.NativeEndian.PutUint64(auxv[0:8], 17) // AT_CLKTCK
	binary.NativeEndian.PutUint64(auxv[8:16], 100)
	if got := parseLinuxAuxvClkTck(auxv[:], 8); got != 100 {
		t.Fatalf("clkTck = %d, want 100", got)
	}
	// Build a realistic /proc/<pid>/stat line with spaces/parens in comm.
	statLine := "4242 (my (sh)) S 1 4242 4242 0 -1 4194304 10 0 0 0 0 0 0 0 20 0 1 0 360000 12345 100"
	p, sid, ok := parseLinuxProcStat(statLine, 1700000000, 100)
	if !ok || p.PID != 4242 || p.Name != "my (sh)" || p.Parent != 1 || p.Group != 4242 || sid != 4242 || p.Terminal || p.Zombie || p.StartTicks != 360000 {
		t.Fatalf("parsed stat: ok=%v sid=%d %+v", ok, sid, p)
	}
	if want := time.Unix(1700003600, 0).UTC(); !p.StartedAt.Equal(want) {
		t.Fatalf("started = %s, want %s", p.StartedAt, want)
	}
	// StartTicks keeps Linux process identity immune to ±1s /proc/stat btime drift.
	pDrifted, _, _ := parseLinuxProcStat(statLine, 1700000001, 100)
	if !sameStart(Finding{PID: 4242, StartedAt: p.StartedAt, StartTicks: p.StartTicks}, pDrifted) {
		t.Fatal("sameStart should match equal StartTicks despite 1s btime drift")
	}
	rUID, eUID, ok := parseLinuxStatusUIDs("Name:\tsh\nUid:\t1000\t1000\t1000\t1000\n")
	if !ok || rUID != 1000 || eUID != 1000 {
		t.Fatalf("uids: %d %d %v", rUID, eUID, ok)
	}
	if _, _, ok := parseLinuxStatusUIDs("Name:\tsh\nUid:\t1000\t1000\t0\t1000\n"); ok {
		t.Fatal("saved UID mismatch should be rejected")
	}
	if !isLinuxSystemdServiceCgroup("0::/user.slice/user-1000.slice/user@1000.service/app.slice/continuum.service\n") {
		t.Fatal("continuum.service should be detected as a systemd service")
	}
	if !isLinuxSystemdServiceCgroup("0::/user.slice/user-1000.slice/user@1000.service/app.slice/continuum.service/worker\n") {
		t.Fatal("delegated sub-cgroup under continuum.service should be detected as a systemd service")
	}
	if isLinuxSystemdServiceCgroup("0::/user.slice/user-1000.slice/session-2.scope\n") {
		t.Fatal("user session scope should not be treated as a systemd service")
	}
	if isLinuxSystemdServiceCgroup("0::/user.slice/user-1000.slice/user@1000.service\n") {
		t.Fatal("top-level user@.service slice should not mark generic user scopes as service units")
	}

	now := time.Unix(1700050000, 0).UTC()
	shProc := process{Name: "sh", StartedAt: now.Add(-2 * time.Hour)}
	if got := describeLinux(shProc, "/home/dev/neo-rsh/crates/neo-launcher", "/tmp/.tmpAb12/launcher.err (deleted)", now, nil); got != launcherReason {
		t.Fatalf("linux launcher = %q, want %q", got, launcherReason)
	}
	if got := describeLinux(shProc, "/tmp/TestCodexExecRequiresStartedTurnForTheTarget_x/001", "/tmp/TestCodexExecRequiresStartedTurnForTheTarget_x/001/native-codex-exec-resume-9.log", now, nil); got != codexReason {
		t.Fatalf("linux codex = %q, want %q", got, codexReason)
	}
	daemonProc := process{Name: "continuum-linux", StartedAt: now.Add(-13 * time.Hour)}
	if got := describeLinux(daemonProc, "/tmp/continuum-hook-test", "/tmp/continuum-hook-test/daemon.stderr.log", now, []string{"/tmp/continuum", "daemon"}); got != daemonReason {
		t.Fatalf("linux daemon = %q, want %q", got, daemonReason)
	}
	if got := describeLinux(daemonProc, "/home/dev/.continuum", "/home/dev/.continuum/daemon.stderr.log", now, []string{"/home/dev/.continuum/bin/continuum", "daemon"}); got != "" {
		t.Fatalf("real user continuum daemon must not match: %q", got)
	}
}

func TestWindowsClassifier(t *testing.T) {
	now := time.Date(2026, 11, 28, 15, 0, 0, 0, time.UTC)
	if got := describeWindows("continuum.exe", `C:\Users\dev\AppData\Local\Temp\continuum-hook-test`, now, now.Add(-13*time.Hour), []string{`C:\temp\continuum.exe`, "daemon"}); got != daemonReason {
		t.Fatalf("windows test daemon = %q, want %q", got, daemonReason)
	}
	if got := describeWindows("continuum.exe", `C:\Users\dev\.continuum`, now, now.Add(-24*time.Hour), []string{`C:\Users\dev\.continuum\bin\continuum.exe`, "daemon"}); got != "" {
		t.Fatalf("real windows daemon must not match: %q", got)
	}
	if got := describeWindows("cmd.exe", `C:\Users\dev\AppData\Local\Temp\TestCodexExecRequiresStartedTurnForTheTarget123\001`, now, now.Add(-2*time.Hour), []string{"cmd.exe", "/c", "native-codex-exec-resume-1.log"}); got != codexReason {
		t.Fatalf("windows codex test shell = %q, want %q", got, codexReason)
	}
	if got := describeWindows("cmd.exe", `C:\Users\dev\AppData\Local\Temp\TestCodexExecRequiresStartedTurnForTheTarget123\001`, now, now.Add(-2*time.Hour), []string{"cmd.exe", "/c", "unrelated"}); got != "" {
		t.Fatalf("unrelated shell without codex log arg should not match: %q", got)
	}
	if got := describeWindows("pwsh.exe", `C:\work\neo-rsh\crates\neo-launcher`, now, now.Add(-2*time.Hour), []string{"pwsh.exe", "-c", `C:\Users\dev\AppData\Local\Temp\.tmpAb12\launcher.err`}); got != launcherReason {
		t.Fatalf("windows launcher = %q, want %q", got, launcherReason)
	}
}
