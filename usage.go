package main

// Usage: how full each agent's context is, and how much of the plan is
// left. Claude Code hands both to its status line command on every refresh,
// so `rex-activity statusline` can sit in front of yours: it notes the
// numbers and runs your command with the same input, so your status line
// looks as it did.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

func stateDir() string {
	if d := os.Getenv("REX_ACTIVITY_STATE"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "rex-activity")
}

// logf notes something in ~/.local/state/rex-activity/activity.log.
func logf(format string, args ...any) {
	path := filepath.Join(stateDir(), "activity.log")
	os.MkdirAll(stateDir(), 0o700)
	if fi, err := os.Stat(path); err == nil && fi.Size() > 1<<20 {
		os.Rename(path, path+".old")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s [%d] %s\n", time.Now().Format("2006-01-02 15:04:05"), os.Getpid(), fmt.Sprintf(format, args...))
}

// keepGoing runs loop, and runs it again if it panics, noting why.
func keepGoing(name string, loop func()) {
	go func() {
		for {
			func() {
				defer func() {
					if r := recover(); r != nil {
						logf("%s crashed: %v\n%s", name, r, debug.Stack())
						time.Sleep(5 * time.Second)
					}
				}()
				loop()
			}()
		}
	}()
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// safeName makes s safe as a file name.
func safeName(s string) string {
	b := []byte(s)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			b[i] = '_'
		}
	}
	return string(b)
}

func itoa(n int) string { return strconv.Itoa(n) }

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// oneLine folds s to one line of at most n characters.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimRight(string(r[:n-1]), " ,.;:") + "…"
}

// contextUse is how full an agent's context window is.
type contextUse struct {
	Tokens int `json:"tokens"`
	Window int `json:"window"`
}

func (c contextUse) frac() float64 {
	if c.Window <= 0 {
		return 0
	}
	return min(1, float64(c.Tokens)/float64(c.Window))
}

// planWindow is one of the plan's usage windows: the 5-hour or the weekly.
type planWindow struct {
	Used    float64   `json:"used"` // percent, 0-100
	ResetAt time.Time `json:"reset_at"`
}

type planUsage struct {
	FiveHour *planWindow `json:"five_hour,omitempty"`
	Weekly   *planWindow `json:"weekly,omitempty"`
	Updated  time.Time   `json:"updated"`
}

func usageDir() string { return filepath.Join(stateDir(), "context") }
func planPath() string { return filepath.Join(stateDir(), "plan.json") }

// runStatusline is `rex-activity statusline [COMMAND...]`.
func runStatusline(args []string) error {
	in, _ := io.ReadAll(os.Stdin)
	noteStatus(in)
	if len(args) == 0 {
		return nil
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin = bytes.NewReader(in)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		return err
	}
	return nil
}

// noteStatus keeps what a status line input says about usage. Anything it
// can't read is left out; the status line still runs.
func noteStatus(in []byte) {
	var st struct {
		SessionID     string `json:"session_id"`
		ContextWindow struct {
			Size         int     `json:"context_window_size"`
			UsedPercent  float64 `json:"used_percentage"`
			CurrentUsage *struct {
				Input       int `json:"input_tokens"`
				CacheCreate int `json:"cache_creation_input_tokens"`
				CacheRead   int `json:"cache_read_input_tokens"`
			} `json:"current_usage"`
		} `json:"context_window"`
		RateLimits map[string]struct {
			Used    float64         `json:"used_percentage"`
			ResetAt json.RawMessage `json:"resets_at"`
		} `json:"rate_limits"`
	}
	if json.Unmarshal(in, &st) != nil || st.SessionID == "" {
		return
	}
	if cw := st.ContextWindow; cw.Size > 0 {
		c := contextUse{Window: cw.Size}
		switch {
		case cw.CurrentUsage != nil:
			c.Tokens = cw.CurrentUsage.Input + cw.CurrentUsage.CacheCreate + cw.CurrentUsage.CacheRead
		case cw.UsedPercent > 0:
			c.Tokens = int(cw.UsedPercent / 100 * float64(cw.Size))
		}
		if data, err := json.Marshal(c); err == nil {
			writeAtomic(filepath.Join(usageDir(), safeName(st.SessionID)+".json"), data)
		}
	}
	if len(st.RateLimits) > 0 {
		p := planUsage{Updated: time.Now()}
		for name, w := range st.RateLimits {
			pw := &planWindow{Used: w.Used, ResetAt: parseWhen(w.ResetAt)}
			switch name {
			case "five_hour":
				p.FiveHour = pw
			case "seven_day":
				p.Weekly = pw
			}
		}
		if data, err := json.Marshal(p); err == nil {
			writeAtomic(planPath(), data)
		}
	}
}

// parseWhen reads a time given as Unix seconds or as RFC 3339.
func parseWhen(raw json.RawMessage) time.Time {
	s := strings.Trim(string(raw), `"`)
	if n, err := strconv.ParseFloat(s, 64); err == nil && n > 0 {
		if n > 1e12 { // milliseconds
			n /= 1000
		}
		return time.Unix(int64(n), 0)
	}
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

// loadPlan is the plan's usage, while it's recent enough to trust.
func loadPlan() (planUsage, bool) {
	var p planUsage
	data, err := os.ReadFile(planPath())
	if err != nil || json.Unmarshal(data, &p) != nil || time.Since(p.Updated) > 12*time.Hour {
		return p, false
	}
	return p, p.FiveHour != nil || p.Weekly != nil
}

// statusContext is the context size the status line last reported for a
// Claude Code session.
func statusContext(session string) (contextUse, bool) {
	var c contextUse
	data, err := os.ReadFile(filepath.Join(usageDir(), safeName(session)+".json"))
	if err != nil || json.Unmarshal(data, &c) != nil || c.Window <= 0 {
		return c, false
	}
	return c, true
}
