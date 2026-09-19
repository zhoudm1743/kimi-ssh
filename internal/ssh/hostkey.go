package ssh

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// AcceptNewHostKeyEnv relaxes host key verification to trust on first use: a
// host that has no recorded key is accepted and appended to ~/.ssh/known_hosts.
// A host recorded with a different key is still refused.
const AcceptNewHostKeyEnv = "KIMI_SSH_ACCEPT_NEW_HOST_KEY"

// hostKeyCallback verifies the remote host key against the known_hosts files
// OpenSSH consults, so a spoofed or swapped key is refused instead of the
// connection trusting whatever answered.
func hostKeyCallback() (ssh.HostKeyCallback, error) {
	files := existingKnownHostsFiles()
	if len(files) == 0 {
		return nil, fmt.Errorf("no known_hosts file found (looked for %s): connect to the host once with the ssh client to record its key, or set %s=1 to trust unknown hosts on first use",
			strings.Join(knownHostsPaths(), ", "), AcceptNewHostKeyEnv)
	}

	verify, err := knownhosts.New(files...)
	if err != nil {
		return nil, fmt.Errorf("failed to read known_hosts: %w", err)
	}

	if !acceptNewHostKey() {
		return verify, nil
	}

	// knownhosts.New snapshots the files when it is built, so remember the keys
	// accepted here instead of appending the same line again on every connect.
	var (
		mu      sync.Mutex
		trusted = make(map[string]bool)
	)

	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		fingerprint := hostname + " " + ssh.FingerprintSHA256(key)

		mu.Lock()
		accepted := trusted[fingerprint]
		mu.Unlock()
		if accepted {
			return nil
		}

		err := verify(hostname, remote, key)
		if err == nil {
			return nil
		}

		var keyErr *knownhosts.KeyError
		if !errors.As(err, &keyErr) || len(keyErr.Want) > 0 {
			// The host is already recorded with another key. Refuse it even when
			// unknown hosts are trusted, since this is the case worth catching.
			return err
		}

		if err := recordHostKey(userKnownHostsFile(), hostname, key); err != nil {
			return err
		}

		mu.Lock()
		trusted[fingerprint] = true
		mu.Unlock()
		return nil
	}, nil
}

// knownHostsPaths lists the host key files OpenSSH consults, in its order.
func knownHostsPaths() []string {
	var paths []string

	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths,
			filepath.Join(home, ".ssh", "known_hosts"),
			filepath.Join(home, ".ssh", "known_hosts2"),
		)
	}

	return append(paths, "/etc/ssh/ssh_known_hosts", "/etc/ssh/ssh_known_hosts2")
}

func existingKnownHostsFiles() []string {
	var files []string
	for _, path := range knownHostsPaths() {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			files = append(files, path)
		}
	}
	return files
}

// userKnownHostsFile is the file newly trusted host keys are appended to.
func userKnownHostsFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.Getenv("HOME"), ".ssh", "known_hosts")
	}
	return filepath.Join(home, ".ssh", "known_hosts")
}

// recordHostKey appends a host key line in the format the ssh client writes.
func recordHostKey(path, hostname string, key ssh.PublicKey) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("host key for %s is not in known_hosts and %s could not be created: %w", hostname, filepath.Dir(path), err)
	}

	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("host key for %s is not in known_hosts and %s could not be opened: %w", hostname, path, err)
	}
	defer file.Close()

	line := knownhosts.Line([]string{knownhosts.Normalize(hostname)}, key)
	if _, err := file.WriteString(line + "\n"); err != nil {
		return fmt.Errorf("failed to record the host key for %s: %w", hostname, err)
	}

	fmt.Fprintf(os.Stderr, "kimi-ssh: trusting new host key for %s, recorded in %s\n", hostname, path)
	return nil
}

func acceptNewHostKey() bool {
	value := os.Getenv(AcceptNewHostKeyEnv)
	return value == "1" || strings.EqualFold(value, "true")
}
