package wrapper

import "testing"

func TestEmptyArgvRejected(t *testing.T) {
	if code, err := Run(Options{}); err == nil || code == 0 {
		t.Fatalf("Run(Options{}) = (%d, %v), want non-zero exit code and error", code, err)
	}
	if _, err := start(nil, nil, 80, 24); err == nil {
		t.Fatal("start(nil, ...) = nil error, want error")
	}
}
