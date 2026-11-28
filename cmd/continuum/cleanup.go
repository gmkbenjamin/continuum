package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/continuum-dev/continuum/internal/cleanup"
	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/store"
)

const (
	cleanupScanFile = "cleanup-scan.json"
	cleanupLockFile = "cleanup.lock"
)

func cmdCleanup(args []string) error {
	fs := newFlagSet("cleanup")
	auto := fs.Bool("auto", false, "With scan, send SIGTERM to the confirmed findings in this run.")
	all := fs.Bool("all", false, "With terminate, select every finding from the specified scan.")
	asJSON := fs.Bool("json", false, "Print the scan or termination results as JSON.")
	action := "scan"
	if len(args) > 0 && (args[0] == "scan" || args[0] == "terminate") {
		action, args = args[0], args[1:]
	}
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	positional := fs.Args()
	var scanID string
	var pids []int
	switch action {
	case "scan":
		if *all || len(positional) != 0 {
			return errors.New("usage: continuum cleanup scan [--auto] [--json]")
		}
	case "terminate":
		if *auto || len(positional) == 0 || (*all != (len(positional) == 1)) {
			return errors.New("usage: continuum cleanup terminate [--json] SCAN_ID PID... or continuum cleanup terminate [--json] --all SCAN_ID")
		}
		scanID = positional[0]
		for _, arg := range positional[1:] {
			pid, err := strconv.Atoi(arg)
			if err != nil || pid <= 1 {
				return fmt.Errorf("invalid PID %q", arg)
			}
			pids = append(pids, pid)
		}
	}
	if err := cleanup.CheckPrivilege(); err != nil {
		return err
	}
	home := core.HomeDir()
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	lock, err := store.Lock(filepath.Join(home, cleanupLockFile))
	if errors.Is(err, store.ErrLocked) {
		return errors.New("another cleanup command is running")
	}
	if err != nil {
		return err
	}
	defer lock.Close()
	path := filepath.Join(home, cleanupScanFile)

	if action == "terminate" {
		scan, err := loadCleanupScan(path, scanID)
		if err != nil {
			return err
		}
		selected, err := scan.Select(pids, *all)
		if err != nil {
			return err
		}
		return runCleanupTermination(scan, selected, *asJSON)
	}

	scan, err := cleanup.Find()
	if err != nil {
		return err
	}
	if *auto {
		return runCleanupTermination(scan, scan.Findings, *asJSON)
	}
	data, err := json.MarshalIndent(scan, "", "  ")
	if err != nil {
		return err
	}
	if err := core.WriteFileAtomic(path, append(data, '\n'), 0o600); err != nil {
		return err
	}
	if *asJSON {
		_, err := fmt.Println(string(data))
		return err
	}
	printCleanupFindings(scan, scan.Findings)
	if len(scan.Findings) > 0 {
		fmt.Printf("After review: continuum cleanup terminate %s PID...\n", scan.ID)
	}
	return nil
}

func loadCleanupScan(path, id string) (cleanup.Scan, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cleanup.Scan{}, errors.New("no saved scan; run continuum cleanup scan")
	}
	if err != nil {
		return cleanup.Scan{}, err
	}
	var scan cleanup.Scan
	if err := json.Unmarshal(data, &scan); err != nil {
		return cleanup.Scan{}, fmt.Errorf("read saved cleanup scan: %w", err)
	}
	if scan.ID == "" || scan.ID != id {
		return cleanup.Scan{}, errors.New("scan ID does not match the latest saved scan; run continuum cleanup scan again")
	}
	return scan, nil
}

func printCleanupFindings(scan cleanup.Scan, findings []cleanup.Finding) {
	fmt.Printf("Scan %s · %d leftover process group(s)\n", scan.ID, len(findings))
	for _, f := range findings {
		fmt.Printf("  PID %d · %s · %s · %d process(es) · %s\n",
			f.PID, f.Command, scan.At.Sub(f.StartedAt).Truncate(time.Minute), f.Members, f.Reason)
		if f.Cwd != "" {
			fmt.Printf("    Working directory %q\n", f.Cwd)
		}
		if f.Stderr != "" {
			fmt.Printf("    Error log %q\n", f.Stderr)
		}
	}
}

func runCleanupTermination(scan cleanup.Scan, selected []cleanup.Finding, asJSON bool) error {
	results, err := scan.Terminate(selected)
	if err != nil && results == nil {
		return err
	}
	if asJSON {
		output := struct {
			ScanID   string            `json:"scan_id"`
			Findings []cleanup.Finding `json:"findings"`
			Results  []cleanup.Result  `json:"results"`
		}{scan.ID, selected, results}
		if output.Findings == nil {
			output.Findings = []cleanup.Finding{}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		err = errors.Join(err, enc.Encode(output))
	} else {
		printCleanupFindings(scan, selected)
		for _, r := range results {
			if r.Detail == "" {
				fmt.Printf("  PID %d: %s\n", r.PID, r.Status)
			} else {
				fmt.Printf("  PID %d: %s (%s)\n", r.PID, r.Status, r.Detail)
			}
		}
	}
	for _, r := range results {
		if r.Status == "failed" || r.Status == "still_running" {
			return errors.Join(err, errors.New("one or more process groups could not be confirmed stopped"))
		}
	}
	return err
}
