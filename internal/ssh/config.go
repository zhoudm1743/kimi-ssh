package ssh

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	// defaultPort is used for a host block that never sets Port, as OpenSSH does.
	defaultPort = "22"
	// maxIncludeDepth bounds nested Include directives so a config that ends up
	// including itself cannot be read forever.
	maxIncludeDepth = 8
	// serverAliveUnset means no block ever mentioned ServerAliveInterval, so the
	// keepalive default applies. An explicit 0 turns keepalives off.
	serverAliveUnset = -1
)

// SSHConfig holds the settings that apply to one host alias, after every
// matching Host block has been folded into it.
type SSHConfig struct {
	Host                string
	HostName            string
	Port                string
	User                string
	IdentityFile        string
	ProxyJump           string
	ProxyCommand        string
	ServerAliveInterval int
}

// setting is one keyword and value in the order it appeared in the file.
type setting struct {
	keyword string
	value   string
}

// hostBlock is one Host line together with the directives that follow it.
// Directives stay in a list rather than being written into a struct right away:
// a block can only be applied once the alias it is matched against is known,
// and a block written for "Host *" applies to every alias.
type hostBlock struct {
	patterns   []string
	directives []setting
}

// appliesTo reports whether the block covers the alias. A negated pattern that
// matches rules the block out even when another pattern on the same line also
// matches, which is how OpenSSH documents negation.
func (b *hostBlock) appliesTo(alias string) bool {
	matched := false

	for _, pattern := range b.patterns {
		if negated := strings.HasPrefix(pattern, "!"); negated {
			if globMatch(strings.TrimPrefix(pattern, "!"), alias) {
				return false
			}
			continue
		}
		if globMatch(pattern, alias) {
			matched = true
		}
	}

	return matched
}

// literalAlias returns the alias a pattern names, when it names exactly one.
// Patterns with wildcards stand for a class of hosts, and a negated pattern
// only ever excludes, so neither is a host to connect to.
func literalAlias(pattern string) (string, bool) {
	if pattern == "" || strings.HasPrefix(pattern, "!") {
		return "", false
	}
	if strings.ContainsAny(pattern, "*?") {
		return "", false
	}
	return pattern, true
}

// globMatch matches an alias against one host pattern: '*' stands for any run
// of characters (dots included), '?' for a single character, and the comparison
// ignores case, as OpenSSH's does.
func globMatch(pattern, alias string) bool {
	pattern = strings.ToLower(pattern)
	alias = strings.ToLower(alias)

	var (
		p      int
		a      int
		star   = -1
		resume int
	)

	for a < len(alias) {
		switch {
		case p < len(pattern) && (pattern[p] == '?' || pattern[p] == alias[a]):
			p++
			a++
		case p < len(pattern) && pattern[p] == '*':
			star = p
			resume = a
			p++
		case star >= 0:
			// Backtrack: let the last '*' swallow one more character.
			resume++
			p = star + 1
			a = resume
		default:
			return false
		}
	}

	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}

// ConfigParser reads host blocks from the OpenSSH client configuration and
// resolves them into one entry per connectable alias.
type ConfigParser struct {
	blocks  []*hostBlock
	configs map[string]*SSHConfig
	order   []string
}

// NewConfigParser returns a parser with nothing loaded yet.
func NewConfigParser() *ConfigParser {
	return &ConfigParser{
		configs: map[string]*SSHConfig{},
	}
}

// ParseConfigFiles loads ~/.ssh/config, every ~/.ssh/config.d/*.conf, and any
// file pulled in by an Include directive. Files that do not exist are skipped;
// a file that exists but cannot be read is reported.
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

	state := &parseState{}
	loaded := map[string]bool{}

	sources := append([]string{filepath.Join(sshDir, "config")}, extra...)
	for _, source := range sources {
		if err := p.loadFile(state, source, sshDir, 0, loaded); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("failed to parse config file %s: %w", source, err)
		}
	}

	p.resolveAll()
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

// parseState carries the block being filled across nested files, so an Include
// inside a host block keeps adding to that block as OpenSSH does.
type parseState struct {
	current *hostBlock
}

// loadFile reads one config file. Directives land in the block opened by the
// most recent Host line; text before the first Host line has nowhere to go and
// is ignored, as before.
func (p *ConfigParser) loadFile(state *parseState, path, sshDir string, depth int, loaded map[string]bool) error {
	if depth > maxIncludeDepth {
		// A config nested this deep is almost certainly a mistake; stop
		// descending but keep the hosts already read, which are still usable.
		fmt.Fprintf(os.Stderr, "kimi-ssh: not following Include in %s: nesting is deeper than %d levels\n", path, maxIncludeDepth)
		return nil
	}

	if absolute, err := filepath.Abs(path); err == nil {
		// A file reached twice - through an Include loop, or through both an
		// Include and the config.d scan - is only read once.
		if loaded[absolute] {
			return nil
		}
		loaded[absolute] = true
	}

	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	lines := bufio.NewScanner(file)
	for lines.Scan() {
		keyword, value, ok := splitDirective(lines.Text())
		if !ok {
			continue
		}

		switch keyword {
		case "host":
			state.current = p.openBlock(value)
		case "include":
			if err := p.loadIncludes(state, value, sshDir, depth, loaded); err != nil {
				return err
			}
		default:
			if state.current != nil {
				state.current.directives = append(state.current.directives, setting{keyword: keyword, value: value})
			}
		}
	}

	return lines.Err()
}

// loadIncludes reads every file an Include directive names. Relative paths are
// resolved against ~/.ssh and a pattern may use shell wildcards; a name that
// matches nothing is skipped rather than failing the whole load.
func (p *ConfigParser) loadIncludes(state *parseState, value, sshDir string, depth int, loaded map[string]bool) error {
	for _, pattern := range strings.Fields(value) {
		for _, target := range includeTargets(pattern, sshDir) {
			if err := p.loadFile(state, target, sshDir, depth+1, loaded); err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return fmt.Errorf("failed to parse included file %s: %w", target, err)
			}
		}
	}
	return nil
}

// includeTargets expands one Include argument into the files it names.
func includeTargets(pattern, sshDir string) []string {
	path := expandTilde(pattern)
	if !filepath.IsAbs(path) {
		path = filepath.Join(sshDir, path)
	}

	if !strings.ContainsAny(path, "*?[") {
		return []string{path}
	}

	matches, err := filepath.Glob(path)
	if err != nil {
		return []string{path}
	}
	return matches
}

// openBlock registers a Host line and returns the block that following
// directives belong to.
func (p *ConfigParser) openBlock(patterns string) *hostBlock {
	block := &hostBlock{patterns: strings.Fields(patterns)}
	p.blocks = append(p.blocks, block)
	return block
}

// resolveAll folds the block list into one SSHConfig per connectable alias.
// Only aliases named literally become entries; a block of wildcards such as
// "Host *" contributes defaults and nothing to connect to.
func (p *ConfigParser) resolveAll() {
	configs := map[string]*SSHConfig{}
	var order []string
	claimed := map[string]bool{}

	for _, block := range p.blocks {
		for _, pattern := range block.patterns {
			alias, literal := literalAlias(pattern)
			if !literal {
				continue
			}

			folded := strings.ToLower(alias)
			if claimed[folded] {
				continue
			}
			claimed[folded] = true

			configs[alias] = p.resolve(alias)
			order = append(order, alias)
		}
	}

	p.configs = configs
	p.order = order
}

// resolve collects the settings that apply to one alias. Blocks are walked in
// file order and the first block that sets a keyword wins, which is OpenSSH's
// "first obtained value" rule. A bare "Host *" block placed at the top of the
// file therefore beats the host-specific blocks below it - that is what
// OpenSSH does, so it is reproduced here rather than corrected.
func (p *ConfigParser) resolve(alias string) *SSHConfig {
	config := &SSHConfig{
		Host:                alias,
		Port:                defaultPort,
		ServerAliveInterval: serverAliveUnset,
	}

	assigned := map[string]bool{}

	for _, block := range p.blocks {
		if !block.appliesTo(alias) {
			continue
		}

		for _, directive := range block.directives {
			if assigned[directive.keyword] {
				continue
			}
			if applySetting(config, directive.keyword, directive.value) {
				assigned[directive.keyword] = true
			}
		}
	}

	return config
}

// applySetting writes one understood keyword into a host entry and reports
// whether the keyword was understood. Keywords missing from the switch are
// ignored, matching OpenSSH's tolerance for options it does not implement.
func applySetting(config *SSHConfig, keyword, value string) bool {
	switch keyword {
	case "hostname":
		config.HostName = value
	case "port":
		config.Port = value
	case "user":
		config.User = value
	case "identityfile":
		config.IdentityFile = expandTilde(value)
	case "proxyjump":
		config.ProxyJump = value
	case "proxycommand":
		// "ProxyCommand none" cancels a proxy, as in OpenSSH.
		if strings.EqualFold(value, "none") {
			value = ""
		}
		config.ProxyCommand = value
	case "serveraliveinterval":
		seconds, err := strconv.Atoi(value)
		if err != nil {
			return false
		}
		config.ServerAliveInterval = seconds
	default:
		return false
	}

	return true
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

// GetAllConfigs returns every connectable host, keyed by alias.
func (p *ConfigParser) GetAllConfigs() map[string]*SSHConfig {
	return p.configs
}

// OrderedConfigs returns the same hosts in the order their aliases first
// appeared in the files, which keeps ssh_list stable across runs.
func (p *ConfigParser) OrderedConfigs() []*SSHConfig {
	configs := make([]*SSHConfig, 0, len(p.order))
	for _, alias := range p.order {
		configs = append(configs, p.configs[alias])
	}
	return configs
}

// GetConfig returns the settings for one alias and whether it is known. Matching
// ignores case, like host matching itself.
func (p *ConfigParser) GetConfig(host string) (*SSHConfig, bool) {
	if config, known := p.configs[host]; known {
		return config, true
	}

	for _, alias := range p.order {
		if strings.EqualFold(alias, host) {
			return p.configs[alias], true
		}
	}

	return nil, false
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
