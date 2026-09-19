package ssh

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// defaultPort is used for a host block that never sets Port, as OpenSSH does.
const defaultPort = "22"

// SSHConfig holds the settings that apply to one host alias.
type SSHConfig struct {
	Host         string
	HostName     string
	Port         string
	User         string
	IdentityFile string
	ProxyJump    string
	ProxyCommand string
}

// ConfigParser reads host blocks from the OpenSSH client configuration.
type ConfigParser struct {
	hosts map[string]*SSHConfig
}

// NewConfigParser returns a parser with no hosts loaded yet.
func NewConfigParser() *ConfigParser {
	return &ConfigParser{hosts: map[string]*SSHConfig{}}
}

// ParseConfigFiles loads ~/.ssh/config and then every ~/.ssh/config.d/*.conf.
// Files that do not exist are skipped; a file that exists but cannot be read is
// reported.
func (p *ConfigParser) ParseConfigFiles() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	sshDir := filepath.Join(home, ".ssh")

	extra, err := confFiles(filepath.Join(sshDir, "config.d"))
	if err != nil {
		return err
	}

	sources := append([]string{filepath.Join(sshDir, "config")}, extra...)
	for _, source := range sources {
		if err := p.load(source); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("failed to parse config file %s: %w", source, err)
		}
	}

	return nil
}

// confFiles lists the *.conf files directly inside dir, sorted for a stable
// load order. A missing directory yields no files.
func confFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read config.d directory %s: %w", dir, err)
	}

	var files []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".conf") {
			continue
		}
		files = append(files, filepath.Join(dir, entry.Name()))
	}

	sort.Strings(files)
	return files, nil
}

// load reads one config file. Directives are written to the aliases named by
// the most recent Host line; text before the first Host line has nowhere to go
// and is ignored.
func (p *ConfigParser) load(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	var block []*SSHConfig

	lines := bufio.NewScanner(file)
	for lines.Scan() {
		keyword, value, ok := splitDirective(lines.Text())
		if !ok {
			continue
		}

		if keyword == "host" {
			block = p.openBlock(value)
			continue
		}

		applyDirective(block, keyword, value)
	}

	return lines.Err()
}

// openBlock registers every pattern on a Host line and returns the entries that
// following directives belong to. An alias seen before keeps the values already
// collected for it.
func (p *ConfigParser) openBlock(patterns string) []*SSHConfig {
	var block []*SSHConfig

	for _, alias := range strings.Fields(patterns) {
		config, known := p.hosts[alias]
		if !known {
			config = &SSHConfig{Host: alias, Port: defaultPort}
			p.hosts[alias] = config
		}
		block = append(block, config)
	}

	return block
}

// directive writes one keyword's value into a host entry.
type directive func(config *SSHConfig, value string)

// directives is the dispatch table for the keywords that are understood.
// Keywords missing from it are ignored, matching OpenSSH's tolerance for
// options it does not implement.
var directives = map[string]directive{
	"hostname": func(config *SSHConfig, value string) {
		config.HostName = value
	},
	"port": func(config *SSHConfig, value string) {
		config.Port = value
	},
	"user": func(config *SSHConfig, value string) {
		config.User = value
	},
	"identityfile": func(config *SSHConfig, value string) {
		config.IdentityFile = expandTilde(value)
	},
	"proxyjump": func(config *SSHConfig, value string) {
		config.ProxyJump = value
	},
	"proxycommand": func(config *SSHConfig, value string) {
		// "ProxyCommand none" cancels a proxy, as in OpenSSH.
		if strings.EqualFold(value, "none") {
			value = ""
		}
		config.ProxyCommand = value
	},
}

// applyDirective writes a keyword's value to every host in the current block.
func applyDirective(block []*SSHConfig, keyword, value string) {
	write, known := directives[keyword]
	if !known {
		return
	}

	for _, config := range block {
		write(config, value)
	}
}

// splitDirective takes one config line apart into a lowercased keyword and its
// value. Comments start at a token beginning with '#' and run to end of line;
// lines without a keyword and a value are dropped.
func splitDirective(line string) (keyword string, value string, ok bool) {
	fields := strings.Fields(line)

	end := len(fields)
	for i, token := range fields {
		if strings.HasPrefix(token, "#") {
			end = i
			break
		}
	}

	if end < 2 {
		return "", "", false
	}

	return strings.ToLower(fields[0]), strings.Join(fields[1:end], " "), true
}

// GetAllConfigs returns every host parsed so far, keyed by alias.
func (p *ConfigParser) GetAllConfigs() map[string]*SSHConfig {
	return p.hosts
}

// GetConfig returns the settings for one alias and whether it is known.
func (p *ConfigParser) GetConfig(host string) (*SSHConfig, bool) {
	config, known := p.hosts[host]
	return config, known
}

// expandTilde turns a leading ~ into the user's home directory, leaving paths
// that do not use it untouched.
func expandTilde(path string) string {
	if !strings.HasPrefix(path, "~") {
		return path
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}

	return filepath.Join(home, strings.TrimPrefix(path, "~"))
}
