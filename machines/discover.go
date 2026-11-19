package machines

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// suggestion is a machine the machines screen offers to add.
type suggestion struct {
	Destination string
	// From is where it came from: ~/.ssh/config, or the discover command.
	From string
}

const (
	fromSSHConfig = "~/.ssh/config"
	fromDiscover  = "discover"
	// maxSuggestions bounds what a discover command or an ssh config can
	// put on the screen.
	maxSuggestions = 64
)

// sshConfigHosts is the Host names in an ssh config and the files it
// includes, in order, leaving out patterns: a name with a wildcard or a
// negation is a rule for many hosts, not a host to connect to.
func sshConfigHosts(path string) []string {
	home, _ := os.UserHomeDir()
	seen := map[string]bool{}
	visited := map[string]bool{}
	var hosts []string
	var read func(path string, depth int)
	read = func(path string, depth int) {
		if depth > 8 || visited[path] || len(hosts) >= maxSuggestions {
			return
		}
		visited[path] = true
		data, err := os.ReadFile(path)
		if err != nil {
			return
		}
		scanner := bufio.NewScanner(bytes.NewReader(data))
		for scanner.Scan() {
			keyword, args := sshConfigLine(scanner.Text())
			switch keyword {
			case "host":
				for _, name := range args {
					if strings.ContainsAny(name, "*?!") || validDestination(name) != nil || seen[name] {
						continue
					}
					seen[name] = true
					hosts = append(hosts, name)
				}
			case "include":
				for _, pattern := range args {
					if rest, ok := strings.CutPrefix(pattern, "~/"); ok {
						pattern = filepath.Join(home, rest)
					} else if !filepath.IsAbs(pattern) {
						// ssh reads a relative Include from ~/.ssh.
						pattern = filepath.Join(home, ".ssh", pattern)
					}
					matches, _ := filepath.Glob(pattern)
					for _, match := range matches {
						read(match, depth+1)
					}
				}
			}
		}
	}
	read(path, 0)
	if len(hosts) > maxSuggestions {
		hosts = hosts[:maxSuggestions]
	}
	return hosts
}

// sshConfigLine is one line's keyword, lower case, and its arguments, the
// way ssh splits them: a keyword then spaces or one "=", and quotes
// around an argument with spaces.
func sshConfigLine(line string) (string, []string) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", nil
	}
	i := strings.IndexAny(line, " \t=")
	if i < 0 {
		return strings.ToLower(line), nil
	}
	keyword := strings.ToLower(line[:i])
	rest := strings.TrimLeft(line[i:], " \t")
	rest = strings.TrimPrefix(rest, "=")
	var args []string
	for rest = strings.TrimSpace(rest); rest != ""; rest = strings.TrimSpace(rest) {
		if rest[0] == '"' {
			end := strings.IndexByte(rest[1:], '"')
			if end < 0 {
				args = append(args, rest[1:])
				break
			}
			args = append(args, rest[1:1+end])
			rest = rest[2+end:]
			continue
		}
		end := strings.IndexAny(rest, " \t")
		if end < 0 {
			args = append(args, rest)
			break
		}
		args = append(args, rest[:end])
		rest = rest[end:]
	}
	return keyword, args
}

// discoverCommand runs the discover setting's command on this machine and
// reads one destination from each line it prints: the first word, with
// blank lines and lines starting with '#' skipped.
func discoverCommand(ctx context.Context, argv []string) ([]string, error) {
	if len(argv) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, &discoverError{err: err, msg: lastLine(msg)}
		}
		return nil, err
	}
	var found []string
	seen := map[string]bool{}
	for _, line := range strings.Split(stdout.String(), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		dest := strings.TrimSuffix(fields[0], ".")
		if validDestination(dest) != nil || seen[dest] {
			continue
		}
		seen[dest] = true
		found = append(found, dest)
		if len(found) == maxSuggestions {
			break
		}
	}
	return found, nil
}

type discoverError struct {
	err error
	msg string
}

func (e *discoverError) Error() string { return e.err.Error() + ": " + e.msg }

// suggestions is every machine the screen can offer that is not on the
// board already, from the discover command and ~/.ssh/config.
// It is slow, so the screen calls it off the board's loop.
func (e *Extension) suggestions(ctx context.Context) ([]suggestion, string) {
	machines := e.machines().all()
	known := map[string]bool{}
	hostnames := map[string]bool{}
	for _, h := range machines {
		known[h.Destination] = true
		// An alias in ~/.ssh/config often names a machine already on the
		// board under another destination.
		if target, ok := e.sshTarget(ctx, h.Destination); ok {
			known[target] = true
		}
		if h.About != nil && h.About.Hostname != "" {
			hostnames[shortHostname(h.About.Hostname)] = true
		}
	}
	// fresh is whether d is not on the board, by its destination or by
	// where ssh would take it, and remembers it so it is offered once.
	fresh := func(d string) bool {
		if known[d] {
			return false
		}
		known[d] = true
		target, ok := e.sshTarget(ctx, d)
		if !ok {
			return true
		}
		if known[target] {
			return false
		}
		known[target] = true
		// A Mac answers on its local network as <hostname>.local, and so
		// do most Linux machines running Avahi, so a host that resolves to
		// the hostname a machine on the board reports is that machine.
		// This is only a suggestion left out: the destination can still be
		// typed into the add form.
		return !hostnames[shortHostname(targetHost(target))]
	}
	var out []suggestion
	problem := ""
	discovered, err := discoverCommand(ctx, e.cfg.Discover)
	if err != nil {
		problem = "the discover command failed: " + err.Error()
	}
	for _, d := range discovered {
		if fresh(d) {
			out = append(out, suggestion{Destination: d, From: fromDiscover})
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		for _, d := range sshConfigHosts(filepath.Join(home, ".ssh", "config")) {
			if fresh(d) {
				out = append(out, suggestion{Destination: d, From: fromSSHConfig})
			}
		}
	}
	return out, problem
}

// sshTarget is where ssh would connect for destination: its user, host and
// port as `ssh -G` resolves them from the operator's ssh config. It reads
// the config only and connects nowhere, so two aliases for one machine
// can be told apart from two machines.
func (e *Extension) sshTarget(ctx context.Context, destination string) (string, bool) {
	if validDestination(destination) != nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := e.run(ctx, append(append([]string{}, e.cfg.SSH...), "-G", "--", destination))
	if err != nil {
		return "", false
	}
	var user, hostname, port string
	for _, line := range strings.Split(string(out), "\n") {
		key, value, _ := strings.Cut(strings.TrimSpace(line), " ")
		switch key {
		case "user":
			user = value
		case "hostname":
			hostname = strings.TrimSuffix(strings.ToLower(value), ".")
		case "port":
			port = value
		}
	}
	if hostname == "" {
		return "", false
	}
	return user + "@" + hostname + ":" + port, true
}

// shortHostname is a hostname compared without case, a trailing dot or a
// ".local" suffix, which is how one machine's own name and its local
// network name match: "dev-mini", "Dev-Mini.local" and
// "dev-mini.local." are one name.
func shortHostname(name string) string {
	name = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
	return strings.TrimSuffix(name, ".local")
}

// targetHost is the host part of an sshTarget.
func targetHost(target string) string {
	_, rest, _ := strings.Cut(target, "@")
	if i := strings.LastIndexByte(rest, ':'); i >= 0 {
		rest = rest[:i]
	}
	return rest
}
