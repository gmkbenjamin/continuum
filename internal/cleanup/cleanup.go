// Package cleanup finds and stops a limited set of recognizable leftover jobs.
package cleanup

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/proc"
)

const (
	launcherReason = "detached neo-launcher test group"
	codexReason    = "detached Continuum Codex test shell"
	daemonReason   = "detached Continuum test daemon"
	logReason      = "log stream with a revoked terminal"
)

type Finding struct {
	PID        int       `json:"pid"`
	Group      int       `json:"group"`
	StartedAt  time.Time `json:"started_at"`
	StartTicks int64     `json:"start_ticks,omitempty"`
	Command    string    `json:"command"`
	Reason     string    `json:"reason"`
	Cwd        string    `json:"cwd"`
	Stderr     string    `json:"stderr,omitempty"`
	Members    int       `json:"members"`
}

type Scan struct {
	ID       string    `json:"id"`
	BootID   string    `json:"boot_id"`
	At       time.Time `json:"at"`
	Findings []Finding `json:"findings"`
}

type Result struct {
	PID    int    `json:"pid"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

type process struct {
	PID, Parent, Group int
	UID, RealUID       int
	Terminal, Zombie   bool
	Name               string
	StartedAt          time.Time
	StartTicks         int64
}

// CheckPrivilege rejects running cleanup as root/setuid (Unix) or an elevated
// Administrator / SYSTEM account (Windows).
func CheckPrivilege() error {
	return checkPrivilege()
}

type inventory map[int]process

// protectedPIDs returns processes that cleanup must never match or signal:
// this process, its ancestor chain, and the active Continuum daemon.
func protectedPIDs() map[int]bool {
	out := map[int]bool{os.Getpid(): true}
	if ppid := os.Getppid(); ppid > 1 {
		out[ppid] = true
	}
	for pid := range proc.Ancestry(os.Getpid()) {
		if pid > 1 {
			out[pid] = true
		}
	}
	if data, err := os.ReadFile(core.GetPaths().PID); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 1 && proc.Alive(pid) {
			out[pid] = true
		}
	}
	return out
}

func memberOlderThanLeader(member, leader process) bool {
	if leader.StartTicks > 0 && member.StartTicks > 0 {
		return member.StartTicks < leader.StartTicks
	}
	return !member.StartedAt.IsZero() && !leader.StartedAt.IsZero() && member.StartedAt.Before(leader.StartedAt)
}

// groupMembers rejects groups with a foreign member, an older member sharing
// the group, a protected/registered member, or a descendant in a different
// group. Only a recognized launcher fixture can have live children.
func (table inventory) groupMembers(leader process, registered map[int]bool, launcher bool) int {
	self := os.Getpid()
	members, shells := 0, 0
	for _, member := range table {
		if member.Group != leader.Group {
			if parent, ok := table[member.Parent]; ok && parent.Group == leader.Group {
				return 0
			}
			continue
		}
		if member.PID == self || member.UID != leader.UID || member.RealUID != leader.UID || member.Terminal || registered[member.PID] {
			return 0
		}
		if member.PID != leader.PID {
			if !launcher || table[member.Parent].Group != leader.Group {
				return 0
			}
			if memberOlderThanLeader(member, leader) {
				return 0
			}
			if member.Name != "sleep" && !isShell(member.Name) {
				return 0
			}
		}
		if isShell(member.Name) && !member.Zombie {
			shells++
		}
		members++
	}
	if launcher && shells < 2 {
		return 0
	}
	return members
}

func isShell(name string) bool {
	return name == "sh" || name == "bash" || name == "dash"
}

// Select validates the entire requested subset before any side effects.
func (scan Scan) Select(pids []int, all bool) ([]Finding, error) {
	if all {
		if len(pids) != 0 {
			return nil, errors.New("use either --all or specific PIDs")
		}
		return scan.Findings, nil
	}
	if len(pids) == 0 {
		return nil, errors.New("select one or more reviewed PIDs, or use --all")
	}
	var selected []Finding
	for _, pid := range pids {
		i := slices.IndexFunc(scan.Findings, func(f Finding) bool { return f.PID == pid })
		if i < 0 {
			return nil, fmt.Errorf("PID %d is absent from scan %s", pid, scan.ID)
		}
		if !slices.ContainsFunc(selected, func(f Finding) bool { return f.PID == pid }) {
			selected = append(selected, scan.Findings[i])
		}
	}
	return selected, nil
}

func currentBoot() (string, error) {
	if err := checkPrivilege(); err != nil {
		return "", err
	}
	return bootID()
}

func Find() (Scan, error) {
	boot, err := currentBoot()
	if err != nil {
		return Scan{}, err
	}
	now := time.Now()
	findings, _, err := discover(now, nil)
	if err != nil {
		return Scan{}, err
	}
	return Scan{ID: core.NewID(), BootID: boot, At: now, Findings: findings}, nil
}

func sameStart(f Finding, p process) bool {
	if f.StartTicks > 0 || p.StartTicks > 0 {
		return f.StartTicks > 0 && f.StartTicks == p.StartTicks
	}
	return p.StartedAt.Equal(f.StartedAt)
}

func sameFinding(a, b Finding) bool {
	sameTime := a.StartedAt.Equal(b.StartedAt)
	if a.StartTicks > 0 || b.StartTicks > 0 {
		sameTime = a.StartTicks > 0 && a.StartTicks == b.StartTicks
	}
	return a.PID == b.PID && a.Group == b.Group && sameTime &&
		a.Reason == b.Reason && a.Cwd == b.Cwd && a.Stderr == b.Stderr && a.Command == b.Command
}

type pendingTarget struct {
	index     int
	finding   Finding
	groupKill bool
}

func targetStillRunning(table inventory, pt pendingTarget) bool {
	if pt.groupKill && pt.finding.Group > 0 {
		for _, p := range table {
			if p.Zombie || p.Group != pt.finding.Group {
				continue
			}
			if pt.finding.StartTicks > 0 && p.StartTicks > 0 {
				if p.StartTicks < pt.finding.StartTicks {
					continue
				}
			} else if !p.StartedAt.IsZero() && !pt.finding.StartedAt.IsZero() && p.StartedAt.Before(pt.finding.StartedAt) {
				continue
			}
			return true
		}
		return false
	}
	p, ok := table[pt.finding.PID]
	if !ok || p.Zombie {
		return false
	}
	return sameStart(pt.finding, p)
}

// Terminate revalidates each approved finding and reports the outcome of stopping it.
func (scan Scan) Terminate(selected []Finding) ([]Result, error) {
	boot, err := currentBoot()
	if err != nil {
		return nil, err
	}
	if boot != scan.BootID {
		return nil, errors.New("this scan belongs to a different boot session; run cleanup scan again")
	}
	results := make([]Result, 0, len(selected))
	if len(selected) == 0 {
		return results, nil
	}
	var pending []pendingTarget
	for _, finding := range selected {
		result := Result{PID: finding.PID, Status: "skipped", Detail: "process exited or the evidence changed"}
		// Re-read the metadata for this specific PID as close to the signal as possible.
		now := time.Now()
		fresh, registered, err := discover(now, &finding)
		switch {
		case err != nil:
			result.Status, result.Detail = "failed", err.Error()
		case len(fresh) > 0 && sameFinding(finding, fresh[0]):
			signaled, err := terminate(now, fresh[0], registered)
			if err != nil {
				result.Status, result.Detail = "failed", err.Error()
			} else if signaled {
				result.Status, result.Detail = "still_running", "termination signal sent"
				pending = append(pending, pendingTarget{
					index:     len(results),
					finding:   fresh[0],
					groupKill: fresh[0].Reason == launcherReason && fresh[0].Group > 0,
				})
			}
		}
		results = append(results, result)
	}
	for deadline := time.Now().Add(2 * time.Second); len(pending) > 0; {
		table, err := snapshot()
		if err != nil {
			return results, fmt.Errorf("could not verify process exit: %w", err)
		}
		pending = slices.DeleteFunc(pending, func(pt pendingTarget) bool {
			if targetStillRunning(table, pt) {
				return false
			}
			results[pt.index].Status, results[pt.index].Detail = "terminated", ""
			return true
		})
		if len(pending) == 0 || !time.Now().Before(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	return results, nil
}
