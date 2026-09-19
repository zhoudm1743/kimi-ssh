package ssh

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// Environment variables that can carry a password. The values are read at
// connect time and are never written to disk, logged or put into an error.
const (
	// PasswordEnv holds a literal password and is the fallback when no askpass
	// program is configured.
	PasswordEnv = "KIMI_SSH_PASSWORD"
	// AskpassEnv names the program OpenSSH-style askpass helpers live in. It is
	// started with the password prompt as its single argument.
	AskpassEnv = "SSH_ASKPASS"
	// AskpassRequireEnv mirrors OpenSSH's setting; the value "never" forbids
	// running the askpass program even when one is configured.
	AskpassRequireEnv = "SSH_ASKPASS_REQUIRE"
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

// defaultIdentityFiles lists the private keys OpenSSH would offer when a host
// block names none, in the order it offers them.
func defaultIdentityFiles() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}

	names := []string{"id_ed25519", "id_ecdsa", "id_rsa", "id_dsa"}
	paths := make([]string, 0, len(names))
	for _, name := range names {
		paths = append(paths, filepath.Join(home, ".ssh", name))
	}
	return paths
}

// identityAuthMethods loads the default private keys that are present. A key
// that cannot be read is skipped rather than aborting the connection: key files
// are often passphrase-protected, and the remaining methods may still work.
func identityAuthMethods() []ssh.AuthMethod {
	var methods []ssh.AuthMethod

	for _, path := range defaultIdentityFiles() {
		if _, err := os.Stat(path); err != nil {
			continue
		}

		signer, err := signerFromFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "kimi-ssh: skipping private key %s: %v\n", path, err)
			continue
		}

		methods = append(methods, ssh.PublicKeys(signer))
	}

	return methods
}

// passwordConfigured reports whether a password could be obtained at all. The
// value itself is only read when a server actually asks for one, so a failed
// public key login does not pop an askpass dialog.
func passwordConfigured() bool {
	if askpassProgram() != "" {
		return true
	}
	return os.Getenv(PasswordEnv) != ""
}

// askpassProgram returns the program a password can be asked from, or "" when
// none may be used.
func askpassProgram() string {
	if strings.EqualFold(strings.TrimSpace(os.Getenv(AskpassRequireEnv)), "never") {
		return ""
	}
	return strings.TrimSpace(os.Getenv(AskpassEnv))
}

// passwordAuthMethods returns the auth methods a configured password is offered
// with, or nothing when no password source exists. Both "password" and
// "keyboard-interactive" are registered because servers disagree about which of
// the two they accept.
func passwordAuthMethods(user, host string) []ssh.AuthMethod {
	if !passwordConfigured() {
		return nil
	}

	var (
		once     sync.Once
		password string
		have     bool
	)
	load := func() (string, bool) {
		// The askpass program is a process launch, so it runs at most once even
		// though two methods may ask for the same secret.
		once.Do(func() { password, have = lookupPassword(user, host) })
		return password, have
	}

	return []ssh.AuthMethod{
		ssh.PasswordCallback(func() (string, error) {
			value, ok := load()
			if !ok {
				return "", errors.New("no password available from " + askpassProgramOrEnv())
			}
			return value, nil
		}),
		ssh.KeyboardInteractive(func(_, _ string, questions []string, _ []bool) ([]string, error) {
			if len(questions) == 0 {
				return []string{}, nil
			}

			value, ok := load()
			if !ok {
				return nil, errors.New("no password available from " + askpassProgramOrEnv())
			}

			// Servers may ask several questions; the same secret answers all of
			// them, which is what a password prompt would do.
			answers := make([]string, len(questions))
			for i := range answers {
				answers[i] = value
			}
			return answers, nil
		}),
	}
}

// askpassProgramOrEnv names where a password was expected from, for an error
// message that is safe to show: it never contains the secret itself.
func askpassProgramOrEnv() string {
	if program := askpassProgram(); program != "" {
		return program
	}
	return PasswordEnv
}

// lookupPassword reads the password to use. The askpass program comes first;
// when there is none, or it fails without producing a password, the literal
// environment variable is used.
func lookupPassword(user, host string) (string, bool) {
	if program := askpassProgram(); program != "" {
		if password, ok := askpassPassword(program, user, host); ok {
			return password, true
		}
	}

	if password := os.Getenv(PasswordEnv); password != "" {
		return password, true
	}

	return "", false
}

// askpassPassword runs an askpass program the way OpenSSH does: the prompt is
// its only argument and the first line of its output is the password. Programs
// that fail or print nothing count as having no password to give.
func askpassPassword(program, user, host string) (string, bool) {
	command := exec.Command(program, fmt.Sprintf("%s@%s's password: ", user, host))
	// Whatever the helper writes must not leak into this process: stdout is the
	// secret, and stderr is dropped rather than forwarded as a log line.
	command.Stderr = io.Discard

	output, err := command.Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "kimi-ssh: askpass program %s did not return a password: %v\n", program, err)
		return "", false
	}

	password := firstLine(string(output))
	if password == "" {
		return "", false
	}
	return password, true
}

// firstLine returns the first line of text, without its line ending.
func firstLine(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	return strings.TrimSuffix(line, "\r")
}
