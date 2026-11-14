package proc

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestExtraDirsSkipsEmptyBase(t *testing.T) {
	dirs := extraDirs("")
	for _, d := range dirs {
		if !filepath.IsAbs(d) {
			t.Fatalf("extraDirs(\"\") produced non-absolute entry %q in %v", d, dirs)
		}
	}
	if runtime.GOOS == "windows" {
		t.Setenv("LOCALAPPDATA", "")
		for _, d := range extraDirs("") {
			if d == `\Microsoft\WinGet\Links` || !filepath.IsAbs(d) {
				t.Fatalf("unexpected empty LOCALAPPDATA entry %q", d)
			}
		}
	}
}

func TestStartDetachedClosesParentFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix fd reuse check")
	}
	bin := "/bin/sh"
	if _, err := os.Stat(bin); err != nil {
		t.Skip("no /bin/sh")
	}
	probeFD := func() int {
		f, err := os.Open(os.DevNull)
		if err != nil {
			t.Fatal(err)
		}
		fd := int(f.Fd())
		_ = f.Close()
		return fd
	}
	base := probeFD()
	dir := t.TempDir()
	for i := 0; i < 20; i++ {
		logPath := filepath.Join(dir, "child.log")
		if _, err := StartDetached([]string{bin, "-c", "exit 0"}, dir, nil, logPath); err != nil {
			t.Fatalf("StartDetached %d: %v", i, err)
		}
	}
	if got := probeFD(); got > base+2 {
		t.Fatalf("parent leaked descriptors across StartDetached: base fd=%d, after 20 starts fd=%d", base, got)
	}
	for i := 0; i < 20; i++ {
		_, err := StartDetached([]string{filepath.Join(dir, "missing-binary")}, dir, nil, filepath.Join(dir, "fail.log"))
		if err == nil {
			t.Fatal("expected start failure")
		}
	}
	if got := probeFD(); got > base+2 {
		t.Fatalf("parent leaked descriptors on StartDetached failure: base fd=%d, after 20 failures fd=%d", base, got)
	}
}
