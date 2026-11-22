package deliver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/proc"
)

// resumeCodexServer starts a turn only in a thread already loaded by the
// local shared server. Standalone and remote clients keep their existing routes.
func resumeCodexServer(o Options, job *core.Job) (Result, error) {
	if len(job.SessionID) != 36 {
		return Result{}, errSkip
	}
	socket := filepath.Join(o.Config.CodexDir(), "app-server-control", "app-server-control.sock")
	if info, err := os.Stat(socket); err != nil || info.Mode()&os.ModeSocket == 0 {
		return Result{}, errSkip
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "unix", socket)
	}}
	conn, _, err := websocket.Dial(ctx, "ws://localhost", &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: transport},
	})
	if err != nil {
		o.logf("Codex app server: %v", err)
		return Result{}, errSkip
	}
	defer conn.CloseNow()
	conn.SetReadLimit(2 << 20)

	var initialized json.RawMessage
	if err := codexRPC(ctx, conn, 1, "initialize", map[string]any{
		"clientInfo": map[string]string{"name": "continuum", "version": core.Version},
	}, &initialized); err != nil {
		o.logf("Codex app server initialize: %v", err)
		return Result{}, errSkip
	}
	if err := wsjson.Write(ctx, conn, map[string]string{"method": "initialized"}); err != nil {
		return Result{}, errSkip
	}
	var thread struct {
		Thread struct {
			ID     string
			Status struct{ Type string }
		}
	}
	if err := codexRPC(ctx, conn, 2, "thread/read", map[string]any{
		"threadId": job.SessionID, "includeTurns": false,
	}, &thread); err != nil {
		o.logf("Codex app server thread read: %v", err)
		return Result{}, errSkip
	}
	if thread.Thread.ID != job.SessionID {
		return Result{}, errSkip
	}
	switch thread.Thread.Status.Type {
	case "active":
		return Result{}, ErrCodexBusy
	case "idle", "systemError":
	default:
		return Result{}, errSkip
	}
	if o.DryRun {
		return Result{Via: "codex-app-server", Detail: "dry run: would start a turn in the loaded Codex thread"}, nil
	}
	var started struct {
		Turn struct{ ID, Status string }
	}
	err = codexRPC(ctx, conn, 3, "turn/start", map[string]any{
		"threadId": job.SessionID,
		"input":    []map[string]any{{"type": "text", "text": o.Prompt, "text_elements": []any{}}},
	}, &started)
	if err != nil {
		return Result{}, fmt.Errorf("%w: Codex app server: %v", ErrUnconfirmed, err)
	}
	if started.Turn.ID == "" || started.Turn.Status != "inProgress" && started.Turn.Status != "completed" {
		return Result{}, fmt.Errorf("%w: Codex app server did not confirm a started turn", ErrUnconfirmed)
	}
	return Result{Via: "codex-app-server", Detail: "started Codex turn " + started.Turn.ID}, nil
}

func codexRPC(ctx context.Context, conn *websocket.Conn, id int, method string, params any, result any) error {
	if err := wsjson.Write(ctx, conn, map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return err
	}
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		var reply struct {
			ID     json.RawMessage
			Result json.RawMessage
			Error  *struct{ Message string }
		}
		if err := json.Unmarshal(data, &reply); err != nil {
			return err
		}
		if string(reply.ID) != fmt.Sprint(id) {
			continue // notifications and server-initiated requests
		}
		if reply.Error != nil {
			return fmt.Errorf("%s: %s", method, reply.Error.Message)
		}
		return json.Unmarshal(reply.Result, result)
	}
}

// startCodexExec checks the JSON event stream of this launch for its thread
// and a started turn. A launch without confirmation may still act later.
func startCodexExec(argv []string, dir string, env []string, logDir, threadID string, wait time.Duration) (string, error) {
	log, err := os.CreateTemp(logDir, "native-codex-exec-resume-*.log")
	if err != nil {
		return "", err
	}
	logPath := log.Name()
	log.Close()
	pid, err := proc.StartDetached(argv, dir, env, logPath)
	if err != nil {
		os.Remove(logPath)
		return "", err
	}
	deadline := time.Now().Add(wait)
	for {
		if codexExecStarted(logPath, threadID) {
			return fmt.Sprintf("Codex started a turn (pid %d), output in %s", pid, logPath), nil
		}
		if !proc.Alive(pid) || !time.Now().Before(deadline) {
			// Read once more in case the last event arrived just before exit.
			if codexExecStarted(logPath, threadID) {
				return fmt.Sprintf("Codex started a turn (pid %d), output in %s", pid, logPath), nil
			}
			return "", fmt.Errorf("%w: Codex exec did not confirm a turn for this thread; output in %s", ErrUnconfirmed, logPath)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func codexExecStarted(path, threadID string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	scanner := bufio.NewScanner(io.LimitReader(f, 256<<10))
	scanner.Buffer(make([]byte, 4096), 256<<10)
	matching := false
	for scanner.Scan() {
		var event struct {
			Type     string
			ThreadID string `json:"thread_id"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue // stderr shares the log
		}
		switch event.Type {
		case "thread.started":
			matching = event.ThreadID == threadID
		case "turn.started":
			if matching {
				return true
			}
		}
	}
	return false
}
