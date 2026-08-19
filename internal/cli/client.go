// Package cli implements the client-side commands: systemctl, journalctl,
// service, the power verbs, systemd-notify and systemd-detect-virt.
package cli

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"time"

	"docker-systemd/internal/paths"
	"docker-systemd/internal/proto"
)

// Client is a control-socket connection.
type Client struct {
	conn net.Conn
}

// Dial connects to the manager and completes the HELLO handshake.
func Dial(name string) (*Client, error) {
	conn, err := net.DialTimeout("unix", paths.ControlSocket, 5*time.Second)
	if err != nil {
		if legacy, lerr := net.DialTimeout("unix", paths.LegacySocket, time.Second); lerr == nil {
			conn = legacy
		} else {
			// The conventional message, not a Go-formatted dial error
			// (defect D5).
			return nil, fmt.Errorf("Failed to connect to bus: No such file or directory")
		}
	}
	c := &Client{conn: conn}
	if err := proto.WriteJSON(conn, proto.TypeHello, proto.Hello{
		Version: proto.ProtocolVersion, Client: name,
	}); err != nil {
		conn.Close()
		return nil, err
	}
	f, err := proto.Read(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("Failed to connect to bus: %v", err)
	}
	if f.Type == proto.TypeResult {
		var res proto.Result
		_ = proto.ReadJSON(f, &res)
		conn.Close()
		return nil, fmt.Errorf("%s", res.Message)
	}
	if f.Type != proto.TypeHelloAck {
		conn.Close()
		return nil, fmt.Errorf("Unexpected greeting from the manager")
	}
	var ack proto.HelloAck
	_ = proto.ReadJSON(f, &ack)
	if ack.Version != proto.ProtocolVersion {
		conn.Close()
		return nil, fmt.Errorf(
			"protocol version mismatch (client %d, manager %d).\n"+
				"The systemctl binary is stale; re-run the installer or use\n"+
				"/usr/sbin/init-docker-systemd systemctl ...",
			proto.ProtocolVersion, ack.Version)
	}
	return c, nil
}

// Close ends the session.
func (c *Client) Close() { c.conn.Close() }

// Response is what a request returned.
type Response struct {
	Data   json.RawMessage
	Result proto.Result
}

// Do sends a request and consumes frames until RESULT. Progress frames are
// written to stderr unless quiet.
func (c *Client) Do(req proto.Request) (*Response, error) {
	if err := proto.WriteJSON(c.conn, proto.TypeRequest, req); err != nil {
		return nil, err
	}
	resp := &Response{}
	for {
		f, err := proto.Read(c.conn)
		if err != nil {
			return nil, err
		}
		switch f.Type {
		case proto.TypeProgress:
			if !req.Options.Quiet {
				fmt.Fprintln(os.Stderr, string(f.Payload))
			}
		case proto.TypeData:
			resp.Data = append(json.RawMessage(nil), f.Payload...)
		case proto.TypeResult:
			if err := proto.ReadJSON(f, &resp.Result); err != nil {
				return nil, err
			}
			return resp, nil
		default:
			// An unexpected frame type is ignored rather than fatal: v0.5.x
			// called log.Fatalf on any trailing data (defect D4).
		}
	}
}

// DecodeData unmarshals the DATA payload.
func (r *Response) DecodeData(v any) error {
	if len(r.Data) == 0 {
		return fmt.Errorf("no data in the response")
	}
	return json.Unmarshal(r.Data, v)
}

// runRequest is the common path for a verb: dial, send, report, exit code.
func runRequest(clientName string, req proto.Request, onData func(*Response) int) int {
	c, err := Dial(clientName)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer c.Close()
	resp, err := c.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", clientName, err)
		return 1
	}
	if resp.Result.Message != "" && resp.Result.ExitCode != 0 {
		fmt.Fprintln(os.Stderr, resp.Result.Message)
	}
	if onData != nil {
		if code := onData(resp); code != 0 {
			return code
		}
	}
	return resp.Result.ExitCode
}
