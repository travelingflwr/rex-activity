package main

// Finding the agents: every Claude Code and Codex running in a Rex pane, on
// this machine and on each Rex host you're connected to. Rex says what each
// pane runs and what state the program reported (working, waiting on you,
// idle). For Claude Code on this machine, its own files add the rest: the
// task you gave it and how full its context is.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

type agentState int

const (
	stWaiting agentState = iota // needs you: sort order
	stWorking
	stIdle
)

type Agent struct {
	Host      string // the Rex host's label, "" for this machine
	Session   string
	Window    string
	Block     string
	Kind      string // Claude Code or Codex
	State     agentState
	Msg       string // what it's doing, or what it waits on
	Title     string // the task: your last prompt, else the tab's name
	Project   string
	Since     time.Time // when the turn began, or when it stopped
	SessionID string    // Claude Code's own
	Ctx       contextUse
	HasCtx    bool
}

func (a Agent) key() string { return a.Host + "/" + a.Block }

// scanScript runs inside Rex (rex do) and prints every pane's foreground
// program and reported status, as JSON, in one round trip.
const scanScript = `
local out = {}
local term = "com.superlogical.terminal"
local listed = rex.call("session.list", {})
for _, s in ipairs((listed or {}).sessions or {}) do
  local v = rex.call("session.view", { session_id = s.session_id })
  for _, w in ipairs((v or {}).windows or {}) do
    for _, l in ipairs(w.layers or {}) do
      for _, b in ipairs(l.blocks or {}) do
        local p = rex.block.call(term, "process", { block_id = b.block_id })
        local fg = p and p.foreground
        if fg then
          local st = rex.block.call(term, "program_status", { block_id = b.block_id })
          table.insert(out, { session = s.session_id, window = w.window_id, wlabel = w.label or "",
            block = b.block_id, name = fg.name or "", path = fg.path or "", pid = fg.pid or 0,
            cwd = fg.cwd or "", status = st })
        end
      end
    end
  end
end
print(rex.json.encode(out))
`

type scannedPane struct {
	Session string `json:"session"`
	Window  string `json:"window"`
	WLabel  string `json:"wlabel"`
	Block   string `json:"block"`
	Name    string `json:"name"`
	Path    string `json:"path"`
	PID     int    `json:"pid"`
	Cwd     string `json:"cwd"`
	Status  struct {
		Records json.RawMessage `json:"records"`
	} `json:"status"`
}

type statusRecord struct {
	App       string    `json:"app"`
	Effective string    `json:"effective_app"`
	State     string    `json:"state"`
	Msg       string    `json:"msg"`
	Title     string    `json:"title"`
	Updated   time.Time `json:"updated_at"`
}

// rexHosts is the label of every Rex host this machine knows.
func rexHosts() []string {
	out, err := rexOutput(3*time.Second, "", "api", "call", "hosts.list")
	if err != nil {
		return nil
	}
	var res struct {
		Hosts []struct {
			Label string `json:"label"`
		} `json:"hosts"`
	}
	json.Unmarshal(out, &res)
	var labels []string
	for _, h := range res.Hosts {
		if h.Label != "" {
			labels = append(labels, h.Label)
		}
	}
	return labels
}

// scanHost lists the agents on one Rex server.
func scanHost(host string) ([]Agent, error) {
	out, err := rexInput(6*time.Second, host, scanScript, "do", "-")
	if err != nil {
		return nil, err
	}
	var panes []scannedPane
	if err := json.Unmarshal(bytes.TrimSpace(out), &panes); err != nil {
		return nil, err
	}
	var agents []Agent
	for _, p := range panes {
		var rec statusRecord
		var recs []statusRecord
		if json.Unmarshal(p.Status.Records, &recs) == nil && len(recs) > 0 {
			rec = recs[0]
		}
		kind := agentKind(p, rec)
		if kind == "" {
			continue
		}
		a := Agent{
			Host: host, Session: p.Session, Window: p.Window, Block: p.Block, Kind: kind,
			State: parseState(rec.State), Msg: rec.Msg, Project: filepath.Base(p.Cwd), Since: rec.Updated,
			Title: tabName(p.WLabel, p.Window),
		}
		if a.Title == "" {
			a.Title = rec.Title
		}
		if host == "" && kind == "Claude Code" {
			fillClaude(&a, p.PID)
		}
		if a.Title == "" {
			a.Title = a.Project
		}
		agents = append(agents, a)
	}
	return agents, nil
}

func agentKind(p scannedPane, rec statusRecord) string {
	app := strings.ToLower(rec.Effective + " " + rec.App)
	name := strings.ToLower(filepath.Base(p.Name))
	path := strings.ToLower(p.Path)
	switch {
	case strings.Contains(app, "claude"), name == "claude", strings.Contains(path, "/claude/versions/"):
		return "Claude Code"
	case strings.Contains(app, "codex"), strings.HasPrefix(name, "codex"), strings.Contains(path, "codex"):
		return "Codex"
	}
	return ""
}

func parseState(raw string) agentState {
	s := strings.ToLower(raw)
	for _, w := range []string{"wait", "input", "attention", "permission", "approv", "ask", "blocked"} {
		if strings.Contains(s, w) {
			return stWaiting
		}
	}
	for _, w := range []string{"work", "busy", "run", "think", "generat"} {
		if strings.Contains(s, w) {
			return stWorking
		}
	}
	return stIdle
}

// tabName is a name you gave the tab, or "" for Rex's own "Window <id>".
func tabName(label, window string) string {
	label = strings.TrimSpace(label)
	id := strings.ReplaceAll(window, "-", "")
	if len(id) > 8 && label == "Window "+id[len(id)-8:] {
		return ""
	}
	return label
}

// fillClaude adds what Claude Code's own files say: ~/.claude/sessions/<pid>.json
// names the session, whose transcript has your last prompt and the context.
func fillClaude(a *Agent, pid int) {
	home, _ := os.UserHomeDir()
	data, err := os.ReadFile(filepath.Join(home, ".claude", "sessions", itoa(pid)+".json"))
	if err != nil {
		return
	}
	var info struct {
		SessionID string `json:"sessionId"`
		Cwd       string `json:"cwd"`
	}
	if json.Unmarshal(data, &info) != nil || info.SessionID == "" {
		return
	}
	a.SessionID = info.SessionID
	if info.Cwd != "" {
		a.Project = filepath.Base(info.Cwd)
	}
	if c, ok := statusContext(info.SessionID); ok {
		a.Ctx, a.HasCtx = c, true
	}
	t := readTranscript(transcriptPath(home, info.SessionID, info.Cwd))
	if t.prompt != "" {
		a.Title = t.prompt
	}
	if a.State == stWorking && !t.promptAt.IsZero() {
		a.Since = t.promptAt
	}
	if !a.HasCtx && t.ctx.Window > 0 {
		a.Ctx, a.HasCtx = t.ctx, true
	}
}

func transcriptPath(home, session, cwd string) string {
	projects := filepath.Join(home, ".claude", "projects")
	slug := []rune(cwd)
	for i, c := range slug {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			slug[i] = '-'
		}
	}
	direct := filepath.Join(projects, string(slug), session+".jsonl")
	if _, err := os.Stat(direct); err == nil {
		return direct
	}
	found, _ := filepath.Glob(filepath.Join(projects, "*", session+".jsonl"))
	if len(found) > 0 {
		return found[0]
	}
	return ""
}

type transcriptInfo struct {
	prompt   string
	promptAt time.Time
	ctx      contextUse
}

var transcripts = struct {
	sync.Mutex
	m map[string]*cachedTranscript
}{m: map[string]*cachedTranscript{}}

// cachedTranscript is what's been read of a transcript so far: a long
// session's last prompt can be megabytes back, so the whole file is read
// once and then only what's added.
type cachedTranscript struct {
	offset int64
	info   transcriptInfo
}

// readTranscript reads a Claude Code transcript for your last prompt and
// the context size after its last reply.
func readTranscript(path string) transcriptInfo {
	if path == "" {
		return transcriptInfo{}
	}
	fi, err := os.Stat(path)
	if err != nil {
		return transcriptInfo{}
	}
	transcripts.Lock()
	defer transcripts.Unlock()
	c := transcripts.m[path]
	if c == nil || fi.Size() < c.offset {
		c = &cachedTranscript{}
		transcripts.m[path] = c
	}
	if fi.Size() == c.offset {
		return c.info
	}
	f, err := os.Open(path)
	if err != nil {
		return c.info
	}
	defer f.Close()
	f.Seek(c.offset, io.SeekStart)
	rd := bufio.NewReaderSize(f, 256<<10)
	for {
		line, err := rd.ReadBytes('\n')
		if err != nil {
			break // a line still being written waits for the next read
		}
		c.offset += int64(len(line))
		c.info.add(line)
	}
	return c.info
}

func (info *transcriptInfo) add(line []byte) {
	if !bytes.Contains(line, []byte(`"user"`)) && !bytes.Contains(line, []byte(`"usage"`)) {
		return
	}
	var e struct {
		Type      string    `json:"type"`
		Sidechain bool      `json:"isSidechain"`
		Meta      bool      `json:"isMeta"`
		Timestamp time.Time `json:"timestamp"`
		Message   struct {
			Model   string          `json:"model"`
			Content json.RawMessage `json:"content"`
			Usage   *struct {
				Input       int `json:"input_tokens"`
				CacheCreate int `json:"cache_creation_input_tokens"`
				CacheRead   int `json:"cache_read_input_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &e) != nil || e.Sidechain || e.Meta {
		return
	}
	switch e.Type {
	case "user":
		if p := promptText(e.Message.Content); p != "" {
			info.prompt, info.promptAt = p, e.Timestamp
		}
	case "assistant":
		if u := e.Message.Usage; u != nil {
			if tokens := u.Input + u.CacheCreate + u.CacheRead; tokens > 0 {
				window := 200_000
				if tokens > window || strings.Contains(strings.ToLower(e.Message.Model), "1m") {
					window = 1_000_000
				}
				info.ctx = contextUse{Tokens: tokens, Window: window}
			}
		}
	}
}

// promptText is what you typed, from a user entry: not a tool result, a
// command or text Claude Code added.
func promptText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &parts) != nil {
			return ""
		}
		for _, p := range parts {
			if p.Type == "tool_result" {
				return ""
			}
			if p.Type == "text" && s == "" {
				s = p.Text
			}
		}
	}
	s = strings.TrimSpace(imageRef.ReplaceAllString(s, ""))
	if s == "" || strings.HasPrefix(s, "<") || strings.HasPrefix(s, "[Request interrupted") {
		return ""
	}
	return oneLine(s, 140)
}

// --- running rex -----------------------------------------------------------

func rexOutput(timeout time.Duration, host string, args ...string) ([]byte, error) {
	return rexInput(timeout, host, "", args...)
}

func rexInput(timeout time.Duration, host, stdin string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if host != "" {
		args = append([]string{"-S", host}, args...)
	}
	cmd := exec.CommandContext(ctx, rexBin(), append([]string{"--autostart=false"}, args...)...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	return cmd.Output()
}

func rexBin() string {
	if p, err := exec.LookPath("rex"); err == nil {
		return p
	}
	return "/Applications/Rex Beta.app/Contents/Helpers/Rex Server.rex/Contents/MacOS/rex"
}

// focus brings an agent's pane forward in the Rex app.
func focus(a Agent) error {
	_, err := rexOutput(4*time.Second, a.Host, "focus", "-s", a.Session, a.Block)
	return err
}

// --- the scanner -------------------------------------------------------------

// scanner keeps the agent list fresh: this machine often, hosts less so.
type scanner struct {
	mu      sync.Mutex
	local   []Agent
	remote  map[string][]Agent
	hosts   []string
	changed chan struct{}
}

func newScanner() *scanner {
	sc := &scanner{remote: map[string][]Agent{}, changed: make(chan struct{}, 1)}
	keepGoing("agent scan", sc.run)
	return sc
}

func (sc *scanner) poke() {
	select {
	case sc.changed <- struct{}{}:
	default:
	}
}

func (sc *scanner) run() {
	tick := 0
	for {
		if local, err := scanHost(""); err == nil {
			sc.mu.Lock()
			sc.local = local
			sc.mu.Unlock()
			sc.poke()
		}
		if tick%30 == 0 {
			hosts := rexHosts()
			sc.mu.Lock()
			sc.hosts = hosts
			sc.mu.Unlock()
		}
		if tick%3 == 0 {
			sc.mu.Lock()
			hosts := append([]string(nil), sc.hosts...)
			sc.mu.Unlock()
			var wg sync.WaitGroup
			for _, h := range hosts {
				wg.Add(1)
				go func(h string) {
					defer wg.Done()
					agents, err := scanHost(h)
					sc.mu.Lock()
					if err == nil {
						sc.remote[h] = agents
					} else {
						delete(sc.remote, h)
					}
					sc.mu.Unlock()
				}(h)
			}
			wg.Wait()
			sc.poke()
		}
		tick++
		time.Sleep(1500 * time.Millisecond)
	}
}

// agents is everyone, sorted: needs you first, then by time.
func (sc *scanner) agents() []Agent {
	sc.mu.Lock()
	all := append([]Agent(nil), sc.local...)
	for _, h := range sc.hosts {
		all = append(all, sc.remote[h]...)
	}
	sc.mu.Unlock()
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].State != all[j].State {
			return all[i].State < all[j].State
		}
		if all[i].State == stIdle {
			return all[i].Since.After(all[j].Since)
		}
		return all[i].Since.Before(all[j].Since)
	})
	return all
}

// imageRef is how a pasted picture shows in a prompt.
var imageRef = regexp.MustCompile(`\[Image #\d+\]`)
