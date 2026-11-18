package machines

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// suggestion is a machine the machines screen offers to add.
type suggestion struct {
	Destination string
	// From is where it came from: ~/.ssh/config, or the discover command.
	From string
	// Status is what a probe found, or "" for one not probed. Hosts from
	// ~/.ssh/config are not probed until picked: that file also names
	// servers that have nothing to do with this board, and nothing should
	// connect to them unasked. The discover command lists what the
	// operator chose to list, so its entries are probed when the screen
	// opens.
	Status string
	Ready  bool
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
// board already: the discover command's, probed, then ~/.ssh/config's.
// It is slow, so the screen calls it off the board's loop.
func (e *Extension) suggestions(ctx context.Context) ([]suggestion, string) {
	known := map[string]bool{}
	for _, h := range e.machines().all() {
		known[h.Destination] = true
		known[h.Name] = true
	}
	var out []suggestion
	problem := ""
	discovered, err := discoverCommand(ctx, e.cfg.Discover)
	if err != nil {
		problem = "the discover command failed: " + err.Error()
	}
	for _, d := range discovered {
		if !known[d] {
			known[d] = true
			out = append(out, suggestion{Destination: d, From: fromDiscover})
		}
	}
	e.probeSuggestions(ctx, out)
	if home, err := os.UserHomeDir(); err == nil {
		for _, d := range sshConfigHosts(filepath.Join(home, ".ssh", "config")) {
			if !known[d] {
				known[d] = true
				out = append(out, suggestion{Destination: d, From: fromSSHConfig})
			}
		}
	}
	return out, problem
}

// probeSuggestions asks each machine for its sessions, a few at a time,
// over a connection of its own that closes after, and says in a few words
// what it found.
func (e *Extension) probeSuggestions(ctx context.Context, list []suggestion) {
	cfg := e.cfg
	cfg.ControlPath = "none"
	limit := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i := range list {
		wg.Add(1)
		go func() {
			defer wg.Done()
			limit <- struct{}{}
			defer func() { <-limit }()
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			h, err := resolveHost("new", hostSettings{Destination: list[i].Destination})
			if err != nil {
				list[i].Status = err.Error()
				return
			}
			snap, err := pollHost(ctx, e.run, cfg, h)
			list[i].Status, list[i].Ready = probeSummary(snap, err)
		}()
	}
	wg.Wait()
}

// probeSummary is a probe's result in a few words.
func probeSummary(snap snapshot, err error) (string, bool) {
	switch {
	case err == nil && snap.boardRunning():
		return "ready, board running", true
	case err == nil:
		return "ready, board stopped", true
	case notFound(err):
		return "no gate-inbox found there", false
	case sshProblem(err) == fixKey:
		return "needs your SSH key", false
	case sshProblem(err) == fixHostKey:
		return "host key not accepted yet", false
	case strings.Contains(err.Error(), "not a session list"):
		return "runs a gate-inbox without this extension", false
	default:
		return "can't connect", false
	}
}
