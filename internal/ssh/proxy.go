package ssh

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"
)

// proxyCommandConn starts the host's ProxyCommand and returns a connection
// backed by its stdin and stdout, the same way OpenSSH tunnels through it.
// address is the "host:port" this connection stands in for; it is reported as
// the remote address so host key verification sees the host being talked to.
func proxyCommandConn(command, address, hostname, port, user string) (net.Conn, error) {
	expanded := strings.NewReplacer(
		"%h", hostname,
		"%n", hostname,
		"%p", port,
		"%r", user,
	).Replace(command)

	cmd := exec.Command("sh", "-c", expanded)
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to open ProxyCommand stdin: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to open ProxyCommand stdout: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start ProxyCommand %q: %w", expanded, err)
	}

	return &proxyConn{reader: stdout, writer: stdin, cmd: cmd, address: address}, nil
}

// proxyConn adapts a ProxyCommand's stdio pipes to net.Conn. A pipe pair cannot
// carry deadlines, so they are accepted and ignored.
type proxyConn struct {
	reader  io.Reader
	writer  io.WriteCloser
	cmd     *exec.Cmd
	address string
}

func (c *proxyConn) Read(b []byte) (int, error)  { return c.reader.Read(b) }
func (c *proxyConn) Write(b []byte) (int, error) { return c.writer.Write(b) }

func (c *proxyConn) Close() error {
	err := c.writer.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	_ = c.cmd.Wait()
	return err
}

// RemoteAddr reports the target host, not the proxy process: known_hosts
// verification parses this address, and a pipe has no address of its own.
func (c *proxyConn) RemoteAddr() net.Addr { return proxyAddr(c.address) }

func (c *proxyConn) LocalAddr() net.Addr { return proxyAddr("127.0.0.1:0") }

func (c *proxyConn) SetDeadline(time.Time) error      { return nil }
func (c *proxyConn) SetReadDeadline(time.Time) error  { return nil }
func (c *proxyConn) SetWriteDeadline(time.Time) error { return nil }

type proxyAddr string

func (a proxyAddr) Network() string { return "proxycommand" }
func (a proxyAddr) String() string  { return string(a) }
