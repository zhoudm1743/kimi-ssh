package ssh

import (
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	// dialAttempts is how many times a connection is tried before giving up.
	dialAttempts = 3
	// handshakeLimit also bounds the SSH handshake over a ProxyCommand, which
	// ClientConfig.Timeout alone does not cover.
	handshakeLimit = 30 * time.Second
	// keepaliveDefault is how often an idle connection is pinged when the host
	// block does not set ServerAliveInterval.
	keepaliveDefault = 30 * time.Second
)

// Connection is one open SSH client together with the settings it was opened
// with, and whether it is still usable.
type Connection struct {
	client *ssh.Client
	config *SSHConfig
	active bool

	stopKeepalive chan struct{}
	stopOnce      sync.Once
}

// close shuts the client down and marks the entry unusable. The keepalive
// goroutine, if any, is told to stop before the client it pings goes away.
func (c *Connection) close() {
	if c.stopKeepalive != nil {
		c.stopOnce.Do(func() { close(c.stopKeepalive) })
	}
	if c.client != nil {
		_ = c.client.Close()
	}
	c.active = false
}

// startKeepalive pings the server so a connection left idle between tool calls
// is not dropped by a NAT or a firewall. The loop ends when the connection is
// closed or the server stops answering.
func (c *Connection) startKeepalive() {
	interval := keepaliveDefault
	if c.config != nil && c.config.ServerAliveInterval >= 0 {
		interval = time.Duration(c.config.ServerAliveInterval) * time.Second
	}
	if interval <= 0 {
		// ServerAliveInterval 0 turns keepalives off, as it does in OpenSSH.
		return
	}

	stop := make(chan struct{})
	c.stopKeepalive = stop

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if _, _, err := c.client.SendRequest("keepalive@openssh.com", true, nil); err != nil {
					return
				}
			}
		}
	}()
}

// ConnectionManager owns the live connections and the host settings they are
// opened from. Its fields are guarded by mu; work that can block (dialing,
// running a command) is done without holding the lock.
type ConnectionManager struct {
	mu          sync.RWMutex
	connections map[string]*Connection
	configs     map[string]*SSHConfig
}

// NewConnectionManager returns a manager with nothing connected.
func NewConnectionManager() *ConnectionManager {
	return &ConnectionManager{
		connections: map[string]*Connection{},
		configs:     map[string]*SSHConfig{},
	}
}

// SetConfigs replaces the host settings used by later Connect calls.
func (cm *ConnectionManager) SetConfigs(configs map[string]*SSHConfig) {
	if configs == nil {
		configs = map[string]*SSHConfig{}
	}

	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.configs = configs
}

// Connect opens a connection for the alias. Connecting to an alias that is
// already up does nothing, so callers may repeat it freely.
func (cm *ConnectionManager) Connect(host string) error {
	config, known := cm.configFor(host)
	if !known {
		return fmt.Errorf("host '%s' not found in SSH config", host)
	}
	if cm.isActive(host) {
		return nil
	}

	client, err := cm.dial(host, config)
	if err != nil {
		return err
	}

	cm.mu.Lock()
	defer cm.mu.Unlock()

	if current, exists := cm.connections[host]; exists && current.active {
		// Something else connected while this call was dialing; keep that
		// connection and throw this one away.
		_ = client.Close()
		return nil
	}

	connection := &Connection{
		client: client,
		config: config,
		active: true,
	}
	// Start the keepalive before publishing the connection, so no other call
	// can close it while the goroutine is being armed.
	connection.startKeepalive()
	cm.connections[host] = connection
	return nil
}

// Disconnect closes the alias's connection and forgets it.
func (cm *ConnectionManager) Disconnect(host string) error {
	cm.mu.Lock()
	conn, known := cm.connections[host]
	if known {
		delete(cm.connections, host)
	}
	cm.mu.Unlock()

	if !known {
		return fmt.Errorf("not connected to host: %s", host)
	}

	conn.close()
	return nil
}

// GetActiveConnections lists the aliases that are connected, sorted so callers
// see a stable order.
func (cm *ConnectionManager) GetActiveConnections() []string {
	cm.mu.RLock()
	hosts := make([]string, 0, len(cm.connections))
	for host, conn := range cm.connections {
		if conn.active {
			hosts = append(hosts, host)
		}
	}
	cm.mu.RUnlock()

	sort.Strings(hosts)
	return hosts
}

// connection returns an open connection for the alias, or an error when the
// alias is not connected.
func (cm *ConnectionManager) connection(host string) (*Connection, error) {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	conn, known := cm.connections[host]
	if !known || !conn.active {
		return nil, fmt.Errorf("not connected to host: %s", host)
	}
	return conn, nil
}

func (cm *ConnectionManager) configFor(host string) (*SSHConfig, bool) {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	config, known := cm.configs[host]
	return config, known
}

// IsConnected reports whether the host still holds a live connection. Callers
// outside this package use it to avoid advertising a host that has gone away.
func (cm *ConnectionManager) IsConnected(host string) bool {
	return cm.isActive(host)
}

func (cm *ConnectionManager) isActive(host string) bool {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	conn, known := cm.connections[host]
	return known && conn.active
}

// dial opens the connection, retrying a few times before reporting failure. The
// pause between tries grows with the attempt number, and the error reported at
// the end carries the last failure, the host and the number of tries.
func (cm *ConnectionManager) dial(host string, config *SSHConfig) (*ssh.Client, error) {
	endpoint := resolveTarget(config)

	clientSettings, err := clientSettingsFor(config, endpoint.user)
	if err != nil {
		return nil, err
	}

	var lastErr error
	for attempt := 1; attempt <= dialAttempts; attempt++ {
		client, err := openClient(config, endpoint, clientSettings)
		if err == nil {
			return client, nil
		}
		lastErr = err

		if attempt < dialAttempts {
			time.Sleep(time.Duration(attempt*attempt) * time.Second)
		}
	}

	return nil, fmt.Errorf("failed to connect to %s after %d attempts: %w", host, dialAttempts, lastErr)
}

// endpoint is the concrete host, user and address a host block resolves to.
type endpoint struct {
	hostname string
	user     string
	address  string
}

// resolveTarget fills in what a host block left out: an alias without HostName
// is dialed by name, and a block without User connects as the local login name.
func resolveTarget(config *SSHConfig) endpoint {
	hostname := config.HostName
	if hostname == "" {
		hostname = config.Host
	}

	user := config.User
	if user == "" {
		user = currentUsername()
	}

	port := config.Port
	if port == "" {
		port = defaultPort
	}

	return endpoint{
		hostname: hostname,
		user:     user,
		address:  hostname + ":" + port,
	}
}

// clientSettingsFor assembles the client settings: the host's identity file when
// one is configured, the running ssh-agent on top of it, the default key files,
// a password if one is configured, and host key verification against
// known_hosts. Keys come before the password so a host that trusts a key is
// never sent a secret it does not need.
func clientSettingsFor(config *SSHConfig, user string) (*ssh.ClientConfig, error) {
	var auth []ssh.AuthMethod

	if path := config.IdentityFile; path != "" {
		signer, err := signerFromFile(path)
		if err != nil {
			return nil, err
		}
		auth = append(auth, ssh.PublicKeys(signer))
	}

	if agent := agentAuthMethod(); agent != nil {
		auth = append(auth, agent)
	}

	auth = append(auth, identityAuthMethods()...)
	auth = append(auth, passwordAuthMethods(user, config.Host)...)

	verifyHostKey, err := hostKeyCallback()
	if err != nil {
		return nil, err
	}

	return &ssh.ClientConfig{
		User:            user,
		Auth:            auth,
		HostKeyCallback: verifyHostKey,
		Timeout:         handshakeLimit,
	}, nil
}

// signerFromFile loads the private key at path.
func signerFromFile(path string) (ssh.Signer, error) {
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read private key file: %w", err)
	}

	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("failed to parse private key: %w", err)
	}
	return signer, nil
}

// openClient establishes one connection, either straight to the address or
// through the host's ProxyCommand.
func openClient(config *SSHConfig, dst endpoint, settings *ssh.ClientConfig) (*ssh.Client, error) {
	if config.ProxyCommand == "" {
		return ssh.Dial("tcp", dst.address, settings)
	}
	return openThroughProxy(config, dst, settings)
}

// openThroughProxy runs the host's ProxyCommand and completes the SSH handshake
// over its pipe. ClientConfig.Timeout is only honoured by ssh.Dial, so the
// handshake is bounded here as well: a proxy that connects but never speaks SSH
// would otherwise block forever.
func openThroughProxy(config *SSHConfig, dst endpoint, settings *ssh.ClientConfig) (*ssh.Client, error) {
	conn, err := proxyCommandConn(config.ProxyCommand, dst.address, dst.hostname, config.Port, dst.user)
	if err != nil {
		return nil, err
	}

	limit := settings.Timeout
	if limit <= 0 {
		limit = handshakeLimit
	}

	type handshake struct {
		client *ssh.Client
		err    error
	}
	done := make(chan handshake, 1)

	go func() {
		clientConn, chans, reqs, err := ssh.NewClientConn(conn, dst.address, settings)
		if err != nil {
			done <- handshake{err: err}
			return
		}
		done <- handshake{client: ssh.NewClient(clientConn, chans, reqs)}
	}()

	select {
	case result := <-done:
		if result.err != nil {
			_ = conn.Close()
			return nil, result.err
		}
		return result.client, nil
	case <-time.After(limit):
		_ = conn.Close()
		return nil, fmt.Errorf("timed out after %s waiting for the SSH handshake over ProxyCommand", limit)
	}
}
