package logx

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLevelsAndFormats(t *testing.T) {
	var file, term bytes.Buffer
	lg := New(Sink{W: &file, Level: Info}, Sink{W: &term, Level: Debug, Human: true})
	lg.now = func() time.Time { return time.Date(2026, 11, 12, 7, 5, 9, 0, time.UTC) }
	lg.Debugf("checked %d panes", 3)
	lg.Infof("resumed %s", "claude:abc")
	lg.Warnf("odd")
	if strings.Contains(file.String(), "checked") {
		t.Fatal("debug line reached an info sink")
	}
	if !strings.Contains(file.String(), "2026-11-12 07:05:09.000 INFO  resumed claude:abc") || !strings.Contains(file.String(), "WARN  odd") {
		t.Fatalf("file:\n%s", file.String())
	}
	if !strings.Contains(term.String(), "07:05:09  debug checked 3 panes") || !strings.Contains(term.String(), "07:05:09  warning: odd") {
		t.Fatalf("terminal:\n%s", term.String())
	}
	var nilLogger *Logger
	nilLogger.Infof("no panic")
	if ParseLevel("DEBUG") != Debug || ParseLevel("warning") != Warn || ParseLevel("x") != Info {
		t.Fatal("ParseLevel")
	}
}

func TestFileRotates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	f, err := OpenFile(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		f.Write([]byte(strings.Repeat("x", 30) + "\n"))
	}
	f.Close()
	a, _ := os.Stat(path)
	b, err := os.Stat(path + ".1")
	if err != nil || a.Size() > 100 || b.Size() > 100 {
		t.Fatalf("rotation: %v %v %v", a.Size(), b, err)
	}
}

func TestFileConcurrentWriteAndClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	f, err := OpenFile(path, 256)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				_, _ = f.Write([]byte("line of log output\n"))
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = f.Close()
		_ = f.Close()
	}()
	wg.Wait()

	if _, err := f.Write([]byte("after close\n")); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("write after Close() = %v, want os.ErrClosed", err)
	}
}
