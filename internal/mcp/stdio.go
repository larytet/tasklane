package mcp

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
)

// ServeStdio is the stdio transport (`tasklane mcp`): newline-delimited JSON-RPC on stdin and stdout, proxied to the HTTP
// endpoint with the user's token, which never travels in a tool argument. The session id from initialize is carried along.
func ServeStdio(in io.Reader, out io.Writer, endpoint, token string) error {
	var wmu sync.Mutex // answers and server-sent notifications share the one output
	write := func(b []byte) { wmu.Lock(); out.Write(b); wmu.Unlock() }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	streaming := false
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	client := &http.Client{}
	sid := ""
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		req, err := http.NewRequest("POST", endpoint, bytes.NewReader([]byte(line)))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		if sid != "" {
			req.Header.Set("Mcp-Session-Id", sid)
		}
		resp, err := client.Do(req)
		if err != nil {
			write([]byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"cannot reach the Tasklane server"}}` + "\n"))
			continue
		}
		if v := resp.Header.Get("Mcp-Session-Id"); v != "" {
			sid = v
			if !streaming { // the session's event stream carries subscription notifications to the client; a server without one answers 405 and nothing is lost
				streaming = true
				go streamNotifications(ctx, client, endpoint, token, sid, write)
			}
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusAccepted || len(bytes.TrimSpace(body)) == 0 {
			continue // a notification: nothing to write back
		}
		if resp.StatusCode == http.StatusUnauthorized {
			write([]byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32001,"message":"unauthorized: check TASKLANE_TOKEN"}}` + "\n"))
			continue
		}
		write(append(bytes.TrimSpace(body), '\n'))
	}
	return sc.Err()
}

// streamNotifications reads the session's server-sent events and writes each message as a line, until the context ends or the stream does.
func streamNotifications(ctx context.Context, client *http.Client, endpoint, token, sid string, write func([]byte)) {
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Mcp-Session-Id", sid)
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if line := sc.Text(); strings.HasPrefix(line, "data: ") {
			write([]byte(line[6:] + "\n"))
		}
	}
}
