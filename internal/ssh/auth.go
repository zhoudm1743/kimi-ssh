package ssh

import (
	"fmt"
	"net"
	"os"
	"os/user"
	"sync"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

var (
	agentOnce   sync.Once
	agentMethod ssh.AuthMethod
)

// currentUsername returns the local login name, which OpenSSH uses as the
// remote user when the host block does not set one.
func currentUsername() string {
	if current, err := user.Current(); err == nil && current.Username != "" {
		return current.Username
	}
	return os.Getenv("USER")
}

// agentAuthMethod returns an auth method backed by the running ssh-agent, or nil
// when no agent socket is available. The socket is opened once per process and
// kept open for the lifetime of the client.
func agentAuthMethod() ssh.AuthMethod {
	agentOnce.Do(func() {
		socket := os.Getenv("SSH_AUTH_SOCK")
		if socket == "" {
			return
		}

		conn, err := net.Dial("unix", socket)
		if err != nil {
			fmt.Fprintf(os.Stderr, "kimi-ssh: ssh-agent at %s is unreachable: %v\n", socket, err)
			return
		}

		agentMethod = ssh.PublicKeysCallback(agent.NewClient(conn).Signers)
	})

	return agentMethod
}
