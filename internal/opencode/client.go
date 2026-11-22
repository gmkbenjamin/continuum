// Package opencode uses the local OpenCode v2 API shared by the desktop and CLI.
package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"time"
)

const LocationKind = "opencode-api"

var (
	ErrBusy      = errors.New("OpenCode is running this session")
	ErrChanged   = errors.New("the OpenCode session changed, already continued, or has queued work")
	ErrUncertain = errors.New("OpenCode did not confirm receipt of the resume prompt")
	sessionID    = regexp.MustCompile(`^ses_[A-Za-z0-9_-]+$`)
	localHTTP    = &http.Client{
		Transport: &http.Transport{IdleConnTimeout: 30 * time.Second},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
)

type Client struct {
	endpoint string
}

// New accepts only direct loopback server URLs. Server credentials come from
// the environment rather than the URL, which is stored with the resume job.
func New(endpoint string) (*Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("opencode_server must be a loopback HTTP(S) server URL")
	}
	if u.Hostname() == "localhost" {
		port := u.Port()
		u.Host = "127.0.0.1"
		if port != "" {
			u.Host = net.JoinHostPort(u.Host, port)
		}
	}
	if ip := net.ParseIP(u.Hostname()); ip == nil || !ip.IsLoopback() {
		return nil, errors.New("opencode_server must use a loopback address")
	}
	u.Path, u.RawPath = "", ""
	return &Client{endpoint: u.String()}, nil
}

func (c *Client) Endpoint() string { return c.endpoint }

type Session struct {
	ID       string `json:"id"`
	ParentID string `json:"parentID"`
	Time     struct {
		Updated  int64 `json:"updated"`
		Archived int64 `json:"archived"`
	} `json:"time"`
	Location struct {
		Directory string `json:"directory"`
	} `json:"location"`
}

type Failure struct {
	Type         string `json:"type"`
	Message      string `json:"message"`
	Status       int    `json:"status"`
	RetryAfterMS int64  `json:"retryAfterMs"`
}

type Message struct {
	ID      string   `json:"id"`
	Type    string   `json:"type"`
	Outcome string   `json:"outcome"`
	Error   *Failure `json:"error"`
	Time    struct {
		Created int64 `json:"created"`
	} `json:"time"`
}

func (m Message) Failed() bool {
	return m.Type == "idle" && m.Outcome == "failed" && m.Error != nil
}

func (c *Client) Sessions(ctx context.Context) ([]Session, error) {
	return get[[]Session](ctx, c, "/api/session?order=desc&parentID=null&archived=false&limit=20")
}

func (c *Client) Active(ctx context.Context) (map[string]json.RawMessage, error) {
	return get[map[string]json.RawMessage](ctx, c, "/api/session/active")
}

func (c *Client) Latest(ctx context.Context, id string) (Message, error) {
	if !sessionID.MatchString(id) {
		return Message{}, errors.New("invalid OpenCode session ID")
	}
	msgs, err := get[[]Message](ctx, c, "/api/session/"+id+"/message?order=desc&limit=1")
	if err != nil || len(msgs) == 0 {
		return Message{}, err
	}
	return msgs[0], nil
}

// Resume checks that the session still has the same failure and an empty
// inbox before queueing the prompt.
func (c *Client) Resume(ctx context.Context, id, failedMessage, directory, text string) error {
	if !sessionID.MatchString(id) || failedMessage == "" {
		return errors.New("missing OpenCode session or failed message ID")
	}
	path := "/api/session/" + id
	session, err := get[Session](ctx, c, path)
	if err != nil {
		return err
	}
	if session.ID != id || session.ParentID != "" || session.Time.Archived != 0 ||
		session.Location.Directory != directory {
		return ErrChanged
	}
	active, err := c.Active(ctx)
	if err != nil {
		return err
	}
	if _, ok := active[id]; ok {
		return ErrBusy
	}
	inbox, err := get[[]json.RawMessage](ctx, c, path+"/inbox")
	if err != nil {
		return err
	}
	last, err := c.Latest(ctx, id)
	if err != nil {
		return err
	}
	if len(inbox) != 0 || last.ID != failedMessage || !last.Failed() {
		return ErrChanged
	}
	var receipt struct {
		Data struct {
			ID        string `json:"id"`
			SessionID string `json:"sessionID"`
		} `json:"data"`
	}
	status, err := c.request(ctx, http.MethodPost, path+"/prompt", map[string]string{"text": text, "delivery": "queue"}, &receipt)
	if status >= 400 && status < 500 {
		return err
	}
	if err != nil || receipt.Data.ID == "" || receipt.Data.SessionID != id {
		// A transport failure or an invalid receipt may follow admission.
		// Retrying without proof could send the same prompt twice.
		return ErrUncertain
	}
	return nil
}

func get[T any](ctx context.Context, c *Client, path string) (T, error) {
	var result struct{ Data *T }
	_, err := c.request(ctx, http.MethodGet, path, nil, &result)
	if err == nil && result.Data == nil {
		err = errors.New("OpenCode returned no response data")
	}
	if err != nil {
		var zero T
		return zero, err
	}
	return *result.Data, nil
}

func (c *Client) request(ctx context.Context, method, path string, input map[string]string, result any) (int, error) {
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if password := os.Getenv("OPENCODE_PASSWORD"); password != "" {
		req.SetBasicAuth("opencode", password)
	}
	res, err := localHTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return res.StatusCode, fmt.Errorf("OpenCode API returned HTTP %d", res.StatusCode)
	}
	return res.StatusCode, json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(result)
}
