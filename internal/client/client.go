// Package client talks to the muster daemon, spawning it if needed.
package client

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"time"

	"github.com/schuettc/muster/internal/proto"
)

// Call sends one request, dialing socketPath and spawning `muster serve` if the
// socket is dead. Returns the daemon's response.
func Call(socketPath string, req proto.Request) (proto.Response, error) {
	conn, err := dialOrSpawn(socketPath)
	if err != nil {
		return proto.Response{}, err
	}
	defer func() { _ = conn.Close() }()

	line, err := json.Marshal(req)
	if err != nil {
		return proto.Response{}, err
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return proto.Response{}, err
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	if !sc.Scan() {
		return proto.Response{}, fmt.Errorf("no response from daemon")
	}
	var resp proto.Response
	if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
		return proto.Response{}, err
	}
	return resp, nil
}

func dialOrSpawn(socketPath string) (net.Conn, error) {
	if c, err := net.Dial("unix", socketPath); err == nil {
		return c, nil
	}
	// Socket dead: spawn the daemon and wait for it to bind.
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(exe, "serve")
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	_ = cmd.Process.Release()
	for i := 0; i < 50; i++ { // up to ~5s
		if c, err := net.Dial("unix", socketPath); err == nil {
			return c, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil, fmt.Errorf("daemon did not start within timeout")
}
