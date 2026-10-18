package mcp

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"strings"
)

// ServeStdio is the stdio transport (`tasklane mcp`): newline-delimited JSON-RPC on stdin and stdout, proxied to the HTTP
// endpoint with the user's token, which never travels in a tool argument. The session id from initialize is carried along.
func ServeStdio(in io.Reader, out io.Writer, endpoint, token string) error {
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
			out.Write([]byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"cannot reach the Tasklane server"}}` + "\n"))
			continue
		}
		if v := resp.Header.Get("Mcp-Session-Id"); v != "" {
			sid = v
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusAccepted || len(bytes.TrimSpace(body)) == 0 {
			continue // a notification: nothing to write back
		}
		if resp.StatusCode == http.StatusUnauthorized {
			out.Write([]byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32001,"message":"unauthorized: check TASKLANE_TOKEN"}}` + "\n"))
			continue
		}
		out.Write(append(bytes.TrimSpace(body), '\n'))
	}
	return sc.Err()
}
