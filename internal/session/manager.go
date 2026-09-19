package session

import (
	"errors"
	"sort"
	"sync"

	"github.com/zhoudm1743/kimi-ssh/internal/ssh"
)

// ErrNoActiveHost is reported when a command needs a host but none is connected
// and none was named.
var ErrNoActiveHost = errors.New("no active SSH connection, use ssh_connect first")

// SessionManager keeps the notion of "the host the tools are talking to". It
// guards that bookkeeping with its own lock and never holds it across a call
// into the connection manager, which serializes its own state.
type SessionManager struct {
	connections *ssh.ConnectionManager
	mu          sync.RWMutex
	activeHost  string
}

// NewSessionManager wraps a connection manager with active-host tracking.
func NewSessionManager(connections *ssh.ConnectionManager) *SessionManager {
	return &SessionManager{connections: connections}
}

// SetConfigs hands the parsed SSH config to the connection manager.
func (sm *SessionManager) SetConfigs(configs map[string]*ssh.SSHConfig) {
	sm.connections.SetConfigs(configs)
}

// Connect opens a connection and makes that host active. An empty host reuses
// the active one, so callers can repeat a connect without naming it.
func (sm *SessionManager) Connect(host string) error {
	target := host
	if target == "" {
		target = sm.GetActiveHost()
	}
	if target == "" {
		return ErrNoActiveHost
	}

	if err := sm.connections.Connect(target); err != nil {
		return err
	}

	sm.mu.Lock()
	sm.activeHost = target
	sm.mu.Unlock()
	return nil
}

// Disconnect closes a connection. When the host that went away was the active
// one, another live connection takes its place, or the active host is cleared.
func (sm *SessionManager) Disconnect(host string) error {
	err := sm.connections.Disconnect(host)

	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.activeHost == host {
		sm.activeHost = pickActive(sm.connections.GetActiveConnections())
	}
	return err
}

// Execute runs a command against the named host; an empty host means the active
// one.
func (sm *SessionManager) Execute(command, host string) (*ssh.Outcome, error) {
	target := host
	if target == "" {
		target = sm.GetActiveHost()
	}
	if target == "" {
		return nil, ErrNoActiveHost
	}

	return sm.connections.Execute(target, command)
}

// ListActiveConnections lists the hosts that are currently connected.
func (sm *SessionManager) ListActiveConnections() []string {
	return sm.connections.GetActiveConnections()
}

// GetActiveConnections is ListActiveConnections under its older name.
func (sm *SessionManager) GetActiveConnections() []string {
	return sm.ListActiveConnections()
}

// GetActiveHost returns the host commands run against by default. A host whose
// connection has gone away is replaced by another live one, or forgotten, so
// status output cannot claim a connection that no longer exists.
func (sm *SessionManager) GetActiveHost() string {
	sm.mu.RLock()
	host := sm.activeHost
	sm.mu.RUnlock()

	if host == "" || sm.connections.IsConnected(host) {
		return host
	}

	sm.mu.Lock()
	if sm.activeHost == host {
		sm.activeHost = pickActive(sm.connections.GetActiveConnections())
	}
	host = sm.activeHost
	sm.mu.Unlock()
	return host
}

// pickActive chooses the replacement active host. Hosts are sorted first so
// consecutive status calls report the same one.
func pickActive(hosts []string) string {
	if len(hosts) == 0 {
		return ""
	}

	sorted := append([]string(nil), hosts...)
	sort.Strings(sorted)
	return sorted[0]
}
