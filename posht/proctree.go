package main

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// procInfo is one node in the ancestry chain from posht up to the session
// leader (or PID 1). The chain is what tells a receipt reader whether posht
// ran bare, under a posh client, inside zmx, over ssh, etc. — the run
// context the altscroll result has to be interpreted against.
type procInfo struct {
	PID     int    `json:"pid"`
	PPID    int    `json:"ppid"`
	Command string `json:"command"`
}

// processTree walks the PPID chain from the current process upward via ps(1),
// one process per call (portable across macOS BSD ps and Linux procps: both
// honour `-o pid=,ppid=,command= -p <pid>`). It stops at PID 1, at a PID it
// cannot read, or after a sane cap so a PPID cycle can never spin forever.
// ps is always present on the targets posht runs on; on the rare host where
// it is missing the chain comes back with whatever was collected (possibly
// just posht itself), which the receipt still records faithfully.
func processTree() []procInfo {
	var chain []procInfo
	seen := make(map[int]bool)
	pid := os.Getpid()
	for i := 0; i < 64; i++ {
		if pid <= 0 || seen[pid] {
			break
		}
		seen[pid] = true
		info, ok := psLookup(pid)
		if !ok {
			break
		}
		chain = append(chain, info)
		if pid == 1 {
			break
		}
		pid = info.PPID
	}
	return chain
}

// terminalFromTree identifies the real terminal emulator from the process
// ancestry, because $TERM lies — macOS terminals inherit "xterm-kitty" from a
// shell config or a prior kitty session, so iTerm2 and Terminal.app both
// report xterm-kitty. The ancestry command lines do not lie. Matches are
// substring checks against each ancestor's command, nearest process first,
// mapped to a short stable label for filenames. Returns "" when no known
// terminal is found (caller falls back to $TERM).
func terminalFromTree(tree []procInfo) string {
	// Ordered so more specific signatures win over generic ones.
	sigs := []struct{ needle, label string }{
		{"iterm", "iterm2"},
		{"kitty", "kitty"},
		{"alacritty", "alacritty"},
		{"wezterm", "wezterm"},
		{"ghostty", "ghostty"},
		{"tmux", "tmux"},
		{"terminal.app", "terminal-app"},
	}
	for _, p := range tree {
		lower := strings.ToLower(p.Command)
		for _, s := range sigs {
			if strings.Contains(lower, s.needle) {
				return s.label
			}
		}
	}
	return ""
}

// psLookup returns one process's pid/ppid/command via ps. The `=` suffixes
// suppress the header row, so stdout is a single line: "<pid> <ppid> <command
// with spaces…>". Splitting on the first two whitespace runs keeps the rest
// (the command, which itself contains spaces) intact.
func psLookup(pid int) (procInfo, bool) {
	out, err := exec.Command("ps", "-o", "pid=,ppid=,command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return procInfo{}, false
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return procInfo{}, false
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return procInfo{}, false
	}
	gotPID, err := strconv.Atoi(fields[0])
	if err != nil {
		return procInfo{}, false
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return procInfo{}, false
	}
	// Recover the command as everything after the pid and ppid columns,
	// preserving its internal spacing rather than re-joining split fields.
	command := strings.TrimSpace(strings.TrimPrefix(line, fields[0]))
	command = strings.TrimSpace(strings.TrimPrefix(command, fields[1]))
	return procInfo{PID: gotPID, PPID: ppid, Command: command}, true
}
