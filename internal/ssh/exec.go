package ssh

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	// ExecTimeoutEnv overrides how long one command may run, in milliseconds.
	ExecTimeoutEnv = "KIMI_SSH_EXEC_TIMEOUT_MS"
	// ExecMaxOutputEnv overrides how many bytes of stdout and stderr are kept.
	ExecMaxOutputEnv = "KIMI_SSH_EXEC_MAX_OUTPUT_BYTES"

	// defaultExecTimeout keeps a command that never returns from holding a tool
	// call open forever.
	defaultExecTimeout = 120 * time.Second
	// defaultExecMaxOutput caps the bytes of each stream that are held in
	// memory; output past the cap is counted and dropped, not buffered.
	defaultExecMaxOutput = 1 << 20
	// teardownGrace bounds how long a session that was closed on timeout gets to
	// unwind before the connection carrying it is dropped as well.
	teardownGrace = 3 * time.Second
	// remoteKillAfterSeconds is passed to `timeout -k`, so a command that
	// ignores SIGTERM is still killed outright.
	remoteKillAfterSeconds = 5
	// clientDeadlineLead is how much later the client deadline falls than the
	// remote timeout. It must outlast the kill-after window above - a command
	// that ignores SIGTERM is only killed that much later - plus a margin, so
	// the remote side gets to finish the job before the client backstop fires.
	clientDeadlineLead = (remoteKillAfterSeconds + 5) * time.Second
	// remoteTimeoutExitCode is what `timeout` exits with when it killed the
	// command it was running.
	remoteTimeoutExitCode = 124
	// remoteKillExitCode is what `timeout` exits with when that kill needed
	// SIGKILL, which happens when the command survives SIGTERM.
	remoteKillExitCode = 137
	// commandNotFoundExitCode is the status a shell reports for a command it
	// could not find, which is how a host without `timeout` answers.
	commandNotFoundExitCode = 127
)

// Outcome is the result of one command run on a remote host. A non-zero exit
// status is an outcome rather than an error: the command ran, the remote shell
// just disagreed.
type Outcome struct {
	Host   string
	Stdout string
	Stderr string
	// ExitCode is the status the remote command exited with, or -1 when the
	// server never reported one.
	ExitCode int
	// TimedOut reports that the command ran past the deadline and was cut off:
	// either killed remotely by `timeout` or stopped by the client backstop.
	TimedOut bool
	// Timeout is the deadline that applied to the command.
	Timeout time.Duration
	// RemoteKilled reports that the remote `timeout` killed the command's whole
	// process group, so nothing it started is left running on the host.
	RemoteKilled bool
	// NoRemoteTimeout reports that the host has no `timeout` command, so the
	// command was re-run without remote enforcement and only the client
	// deadline stood behind it.
	NoRemoteTimeout bool
	// ConnectionClosed reports that the timeout could not be cleaned up by
	// closing the session alone, so the connection was dropped too.
	ConnectionClosed bool
	// StdoutBytes and StderrBytes count everything the command wrote, including
	// whatever did not fit under the output cap.
	StdoutBytes int64
	StderrBytes int64
}

// StdoutTruncated reports whether stdout was cut off at the output cap.
func (o *Outcome) StdoutTruncated() bool {
	return o.StdoutBytes > int64(len(o.Stdout))
}

// StderrTruncated reports whether stderr was cut off at the output cap.
func (o *Outcome) StderrTruncated() bool {
	return o.StderrBytes > int64(len(o.Stderr))
}

// Execute runs one command on the alias's connection. Output written to stdout
// and stderr is returned even when the command exits non-zero.
func (cm *ConnectionManager) Execute(host, command string) (*Outcome, error) {
	conn, err := cm.connection(host)
	if err != nil {
		return nil, err
	}

	timeout := execTimeout()

	// The remote `timeout` is the primary enforcement: it puts the command in
	// its own process group and kills that entire group when the deadline
	// passes, so the grandchildren a shell would otherwise orphan die with it,
	// the session unwinds on its own and the connection stays usable. The client
	// deadline is only a backstop, and a host that turns out not to have
	// `timeout` gets the command run again without the wrapper.
	outcome, err := cm.run(conn, host, wrapCommand(command, timeout), timeout, true)
	if err != nil {
		return nil, err
	}

	if outcome.ExitCode != commandNotFoundExitCode || !missingRemoteTimeout(outcome.Stderr) {
		return outcome, nil
	}

	// The host could not run the wrapper at all, so run the command as it was
	// given - once. Nothing enforces the deadline remotely in that case, which
	// the result reports rather than leaving it to be read as a guarantee.
	fallback, err := cm.run(conn, host, command, timeout, false)
	if err != nil {
		return nil, err
	}
	fallback.NoRemoteTimeout = true
	return fallback, nil
}

// run performs one attempt over a fresh session. wrapped says whether the
// command carries the remote `timeout` wrapper, which is what gives the timeout
// exit codes their meaning; without it they are ordinary failures.
func (cm *ConnectionManager) run(conn *Connection, host, command string, timeout time.Duration, wrapped bool) (*Outcome, error) {
	remote, err := conn.client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("failed to create SSH session: %w", err)
	}
	defer remote.Close()

	stdout, err := remote.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdout pipe: %w", err)
	}
	stderr, err := remote.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	deadline := time.Now().Add(timeout + clientDeadlineLead)
	outcome := &Outcome{Host: host, Timeout: timeout}

	// Starting the command waits for the server to accept the exec request. A
	// server that takes the channel but never answers would hold the call open
	// forever, so the request is bounded by the same per-command deadline.
	started := make(chan error, 1)
	go func() { started <- remote.Start(command) }()

	select {
	case err := <-started:
		if err != nil {
			return nil, fmt.Errorf("failed to start command: %w", err)
		}
	case <-time.After(time.Until(deadline)):
		outcome.TimedOut = true
		// Nothing is known about the state of the session now, so the whole
		// connection goes with it.
		conn.close()
		cm.forget(host, conn)
		outcome.ConnectionClosed = true
		return outcome, nil
	}

	limit := execMaxOutputBytes()
	out := &cappedBuffer{limit: limit}
	errOut := &cappedBuffer{limit: limit}

	// Drain both pipes for as long as the command runs; a command that fills a
	// pipe buffer would otherwise stall waiting for a reader.
	var drains sync.WaitGroup
	drains.Add(2)
	go func() {
		defer drains.Done()
		_, _ = io.Copy(out, stdout)
	}()
	go func() {
		defer drains.Done()
		_, _ = io.Copy(errOut, stderr)
	}()

	finished := make(chan error, 1)
	go func() { finished <- remote.Wait() }()

	var waitErr error
	select {
	case waitErr = <-finished:
	case <-time.After(time.Until(deadline)):
		// The remote timeout did not stop the command, so this is the backstop
		// path. Ask the server to kill the command and close the session: a
		// signal normally ends the remote process at once. Without a pty it only
		// reaches the direct child, so whatever that child started may keep
		// running; if the session still does not unwind, the connection
		// underneath it cannot be trusted either and is dropped.
		outcome.TimedOut = true
		_ = remote.Signal(ssh.SIGKILL)
		_ = remote.Close()
		if !delivered(finished, teardownGrace) {
			conn.close()
			cm.forget(host, conn)
			outcome.ConnectionClosed = true
			_ = delivered(finished, teardownGrace)
		}
	}

	waitDrained(&drains, teardownGrace)

	outcome.Stdout = out.String()
	outcome.Stderr = errOut.String()
	outcome.StdoutBytes = out.total
	outcome.StderrBytes = errOut.total
	outcome.ExitCode = exitCodeOf(waitErr)

	// A command the remote `timeout` had to kill exits with 124, or with 137 when
	// the kill needed SIGKILL. That is the deadline being enforced on the host,
	// not an ordinary non-zero exit.
	if wrapped && (outcome.ExitCode == remoteTimeoutExitCode || outcome.ExitCode == remoteKillExitCode) {
		outcome.RemoteKilled = true
		outcome.TimedOut = true
	}

	return outcome, nil
}

// forget drops a connection from the manager, but only when it is still the one
// recorded for the alias: another call may have replaced it meanwhile.
func (cm *ConnectionManager) forget(host string, conn *Connection) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	if current, known := cm.connections[host]; known && current == conn {
		delete(cm.connections, host)
	}
}

// delivered reports whether the channel produced a value within the limit.
func delivered(ch <-chan error, limit time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(limit):
		return false
	}
}

// waitDrained waits for the pipe readers, but never longer than the limit: a
// reader that is stuck must not turn into a stuck tool call.
func waitDrained(drains *sync.WaitGroup, limit time.Duration) {
	done := make(chan struct{})
	go func() {
		drains.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(limit):
	}
}

// exitCodeOf reads the remote exit status out of the error the session ended
// with. A server that closed the session without reporting one leaves -1.
func exitCodeOf(waitErr error) int {
	if waitErr == nil {
		return 0
	}

	var exitErr *ssh.ExitError
	if errors.As(waitErr, &exitErr) {
		return exitErr.ExitStatus()
	}

	return -1
}

// cappedBuffer keeps at most limit bytes while still counting everything
// written, so a command that prints without end cannot exhaust memory and the
// caller can still be told how much was dropped.
type cappedBuffer struct {
	buffer bytes.Buffer
	total  int64
	limit  int64
}

// Write records the chunk and keeps as much of it as the cap allows. The full
// length is always reported, so io.Copy never sees a short write.
func (c *cappedBuffer) Write(chunk []byte) (int, error) {
	written := len(chunk)
	c.total += int64(written)

	if room := c.limit - int64(c.buffer.Len()); room > 0 {
		if int64(written) > room {
			chunk = chunk[:room]
		}
		c.buffer.Write(chunk)
	}

	return written, nil
}

func (c *cappedBuffer) String() string { return c.buffer.String() }

// execTimeout is the deadline for one command.
func execTimeout() time.Duration {
	if value, ok := envInt(ExecTimeoutEnv); ok && value > 0 {
		return time.Duration(value) * time.Millisecond
	}
	return defaultExecTimeout
}

// wrapCommand puts the command under the remote `timeout`, which runs it in its
// own process group and kills that whole group once the deadline passes. The
// command is handed to `sh -c` quoted for the login shell that parses this line,
// so pipes, operators, redirections and quotes inside it reach `sh` untouched.
func wrapCommand(command string, timeout time.Duration) string {
	return fmt.Sprintf("timeout -k %d %d sh -c %s",
		remoteKillAfterSeconds, timeoutSeconds(timeout), shellQuote(command))
}

// shellQuote makes text a single word for the remote shell: it is wrapped in
// single quotes, and every embedded quote is closed, escaped and reopened.
func shellQuote(text string) string {
	return "'" + strings.ReplaceAll(text, "'", `'\''`) + "'"
}

// timeoutSeconds rounds up, so a sub-second deadline still gives `timeout` an
// interval it can take and never rounds down to zero.
func timeoutSeconds(timeout time.Duration) int64 {
	seconds := int64(timeout / time.Second)
	if timeout%time.Second != 0 {
		seconds++
	}
	if seconds < 1 {
		seconds = 1
	}
	return seconds
}

// missingRemoteTimeout reports whether an attempt failed because the host has no
// `timeout` command. The shell says so on stderr, and each line is matched
// against the notices the common shells print, so an unrelated failure that
// merely mentions a timeout does not count.
func missingRemoteTimeout(stderr string) bool {
	notices := []string{
		"timeout: not found",
		"timeout: command not found",
		"timeout: no such file",
		"command not found: timeout",
	}

	for _, line := range strings.Split(stderr, "\n") {
		lower := strings.ToLower(line)
		for _, notice := range notices {
			if strings.Contains(lower, notice) {
				return true
			}
		}
	}
	return false
}

// execMaxOutputBytes is the per-stream output cap.
func execMaxOutputBytes() int64 {
	if value, ok := envInt(ExecMaxOutputEnv); ok && value > 0 {
		return value
	}
	return defaultExecMaxOutput
}

// envInt reads an integer setting from the environment. An unset or unparsable
// value leaves the caller's default in place.
func envInt(name string) (int64, bool) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return 0, false
	}

	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}
