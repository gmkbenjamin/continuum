//go:build !windows

package deliver

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/live"
	"github.com/continuum-dev/continuum/internal/rules"
)

func TestCodexResumesLoadedThreadByID(t *testing.T) {
	const id = "01a202ab-358f-70e3-9ff4-b2b19e96e4d1"
	for _, tc := range []struct {
		name, status, id, turnStatus string
		wantErr                      error
	}{
		{name: "stalled", status: "systemError", id: id, turnStatus: "inProgress"},
		{name: "busy", status: "active", id: id, wantErr: ErrCodexBusy},
		{name: "unloaded", status: "notLoaded", id: id, wantErr: ErrStillOpen},
		{name: "wrong thread", status: "idle", id: "another", wantErr: ErrStillOpen},
		{name: "unconfirmed turn", status: "idle", id: id, turnStatus: "failed", wantErr: ErrUnconfirmed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, err := os.MkdirTemp("/tmp", "cont-codex-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(home) })
			control := filepath.Join(home, "app-server-control")
			if err := os.Mkdir(control, 0o700); err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("unix", filepath.Join(control, "app-server-control.sock"))
			if err != nil {
				t.Fatal(err)
			}
			requests := make(chan struct {
				ThreadID string
				Input    []struct{ Text string }
			}, 1)
			done := make(chan struct{})
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				defer close(done)
				conn, err := websocket.Accept(w, req, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.CloseNow()
				for {
					var call struct {
						ID     int
						Method string
						Params json.RawMessage
					}
					if err := wsjson.Read(req.Context(), conn, &call); err != nil {
						return
					}
					var response any
					switch call.Method {
					case "initialize":
						response = map[string]string{}
					case "initialized":
						continue
					case "thread/read":
						var target struct{ ThreadID string }
						if err := json.Unmarshal(call.Params, &target); err != nil || target.ThreadID != id {
							t.Errorf("read requested thread %q: %v", target.ThreadID, err)
							return
						}
						response = map[string]any{"thread": map[string]any{
							"id": tc.id, "status": map[string]string{"type": tc.status},
						}}
					case "turn/start":
						var turn struct {
							ThreadID string
							Input    []struct{ Text string }
						}
						if err := json.Unmarshal(call.Params, &turn); err != nil {
							t.Error(err)
							return
						}
						requests <- turn
						_ = wsjson.Write(req.Context(), conn, map[string]any{"method": "turn/started"})
						response = map[string]any{"turn": map[string]string{"id": "turn-1", "status": tc.turnStatus}}
					default:
						t.Errorf("unexpected request %q", call.Method)
						return
					}
					if err := wsjson.Write(req.Context(), conn, map[string]any{"id": call.ID, "result": response}); err != nil {
						t.Error(err)
						return
					}
				}
			})}
			t.Cleanup(func() { server.Close() })
			go server.Serve(listener)

			bin, calls := fakeAgent(t, t.TempDir(), "codex", "queue exec resume --thread --message --resume --json")
			r, _ := rules.Load("")
			o := Options{
				Rules: r, Config: core.Config{CodexHome: home, Binaries: map[string]string{"codex": bin}},
				Strategies: []string{"wrapper", "native"},
				Prompt:     "Continue",
				Live:       &live.State{Checked: true, Open: true, Source: "processes"},
			}
			res, err := Deliver(o, &core.Job{Harness: "codex", SessionID: id})
			if !errors.Is(err, tc.wantErr) || tc.wantErr == nil && res.Via != "codex-app-server" {
				t.Fatalf("delivery = %+v, %v; want %v", res, err, tc.wantErr)
			}
			<-done
			select {
			case turn := <-requests:
				if tc.turnStatus == "" || turn.ThreadID != id || len(turn.Input) != 1 || turn.Input[0].Text != o.Prompt {
					t.Fatalf("unexpected turn request: %+v", turn)
				}
			default:
				if tc.turnStatus != "" {
					t.Fatal("expected a turn request")
				}
			}
			if _, err := os.Stat(calls); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("a second route invoked the CLI: %v", err)
			}
		})
	}
}

func TestCodexExecRequiresStartedTurnForTheTarget(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		wantErr      error
	}{
		{"started", `{"type":"thread.started","thread_id":"t1"}
{"type":"turn.started"}`, nil},
		{"launch only", `{"type":"thread.started","thread_id":"t1"}`, ErrUnconfirmed},
		{"wrong thread", `{"type":"thread.started","thread_id":"t2"}
{"type":"turn.started"}`, ErrUnconfirmed},
		{"silent", "", ErrUnconfirmed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "codex")
			if err := os.WriteFile(bin, []byte("#!/bin/sh\ncat <<'OUT'\n"+tc.output+"\nOUT\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			_, err := startCodexExec([]string{bin}, dir, nil, dir, "t1", time.Second)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("launch result = %v, want %v", err, tc.wantErr)
			}
		})
	}
}
