package main

// Rex hosts: the Rex version on this machine and on each Rex host, and
// whether a newer one is out. Where a host allows it, Upgrade installs the
// newer rex package and Restart restarts its Rex server; both ask first,
// listing what they'll do. A remote host is checked over SSH (key login,
// BatchMode), its alias the Rex host's label in lower case.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image/color"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

type hostStatus int

const (
	hsChecking hostStatus = iota
	hsUpToDate
	hsUpdate      // a newer rex package is out
	hsRestart     // installed is newer than what's running
	hsMacUpdate   // the Rex app has an update
	hsUnreachable // no answer over Rex
	hsUnknown
	hsWorking
	hsFailed
)

type hostRow struct {
	Label          string // "" for this machine
	Platform       string
	Running        string
	Installed      string
	Latest         string
	App            string // this Mac: the Rex app's version and build
	Sessions       []string
	CanUpgrade     bool
	CanRestart     bool
	RestartBlocked string
	Rexurrect      bool
	Stray          bool // a Rex server without --tailscale holds the socket
	Status         hostStatus
	Note           string // for unknown, working and failed
}

func (h hostRow) name() string {
	if h.Label == "" {
		if runtime.GOOS == "darwin" {
			return "This Mac"
		}
		return "This machine"
	}
	return h.Label
}

type remoteFacts struct {
	installed, suite, arch                     string
	unit, active, rexurrect, sudo, stray, seen bool
}

// strayServers is shell that finds (and stops) this user's Rex servers on
// the default socket without --tailscale: ones a rex command or an app
// autostarted, which keep rex-user.service, and the host's tailnet name,
// from starting.
const strayServers = `
stray_servers() {
    sock=/run/user/$(id -u)/rex/server.sock
    for pid in $(pgrep -u "$(id -u)" -f "rex server run"); do
        cmd=$(tr '\0' ' ' </proc/"$pid"/cmdline 2>/dev/null) || continue
        case " $cmd " in *" --listen unix://$sock "*|*" --listen $sock "*) ;; *) continue ;; esac
        case "$cmd" in *--tailscale*) continue ;; esac
        echo "$pid"
    done
}
stop_stray() {
    for pid in $(stray_servers); do
        echo "==> stopping Rex server $pid, which holds the socket without Tailscale"
        kill -TERM "$pid" 2>/dev/null || true
        i=0; while kill -0 "$pid" 2>/dev/null && [ $i -lt 50 ]; do sleep 0.1; i=$((i+1)); done
        kill -KILL "$pid" 2>/dev/null || true
    done
}
`

const factsScript = strayServers + `
printf 'installed=%s\n' "$(dpkg-query -W -f='${Version}' rex 2>/dev/null)"
printf 'suite=%s\n' "$(sed -n 's/^Suites: *//p' /etc/apt/sources.list.d/rex.sources 2>/dev/null | head -1)"
printf 'arch=%s\n' "$(dpkg --print-architecture 2>/dev/null)"
systemctl --user cat rex-user.service >/dev/null 2>&1 && echo unit=yes
systemctl --user is-active --quiet rex-user.service && echo active=yes
systemctl --user cat rexurrect-autosave.service >/dev/null 2>&1 && echo rexurrect=yes
sudo -n true 2>/dev/null && echo sudo=yes
[ -n "$(stray_servers)" ] && echo stray=yes
exit 0
`

func sshRun(host, script string, timeout time.Duration) (string, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", "-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=6", strings.ToLower(host), "sh -s")
	cmd.Stdin = strings.NewReader(script)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.String(), errb.String(), err
}

func readFacts(host string) (remoteFacts, bool) {
	out, _, err := sshRun(host, factsScript, 20*time.Second)
	if err != nil {
		return remoteFacts{}, false
	}
	f := remoteFacts{seen: true}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok || v == "" {
			continue
		}
		switch k {
		case "installed":
			f.installed = v
		case "suite":
			f.suite = v
		case "arch":
			f.arch = v
		case "unit":
			f.unit = true
		case "active":
			f.active = true
		case "rexurrect":
			f.rexurrect = true
		case "sudo":
			f.sudo = true
		case "stray":
			f.stray = true
		}
	}
	return f, true
}

// latestApt is the newest rex in Superlogical's apt repository for a
// channel and CPU.
func latestApt(suite, arch string) string {
	resp, err := httpClient.Get(fmt.Sprintf("https://pkgs.superlogical.com/rex/apt/dists/%s/main/binary-%s/Packages", suite, arch))
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	best, inRex := "", false
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		l := sc.Text()
		switch {
		case strings.HasPrefix(l, "Package: "):
			inRex = l == "Package: rex"
		case inRex && strings.HasPrefix(l, "Version: "):
			if v := l[9:]; best == "" || compareVersions(best, v) < 0 {
				best = v
			}
		}
	}
	return best
}

const rexApp = "/Applications/Rex Beta.app"

func plistValue(app, key string) string {
	out, err := exec.Command("/usr/bin/plutil", "-extract", key, "raw", app+"/Contents/Info.plist").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

var sparkleVersion = regexp.MustCompile(`<sparkle:version>([^<]+)</sparkle:version>|sparkle:version="([^"]+)"`)

// latestMacBuild is the newest build in the Rex app's own update feed.
func latestMacBuild() string {
	feed := plistValue(rexApp, "SUFeedURL")
	if feed == "" {
		return ""
	}
	resp, err := httpClient.Get(feed)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	b.ReadFrom(resp.Body)
	best := ""
	for _, m := range sparkleVersion.FindAllStringSubmatch(b.String(), -1) {
		v := m[1] + m[2]
		if best == "" || compareVersions(best, v) < 0 {
			best = v
		}
	}
	return best
}

type serverVersion struct {
	Version  string `json:"rex_version"`
	Platform string `json:"platform"`
}

func readServer(host string) (serverVersion, bool) {
	var sv serverVersion
	out, err := rexOutput(5*time.Second, host, "api", "call", "server.version")
	if err != nil || json.Unmarshal(out, &sv) != nil || sv.Version == "" {
		return sv, false
	}
	return sv, true
}

func prettyPlatform(p string) string {
	return strings.NewReplacer("darwin", "macOS", "_", " ").Replace(p)
}

// --- watching the hosts -------------------------------------------------------

type hostsWatch struct {
	mu      sync.Mutex
	rows    []hostRow
	facts   map[string]remoteFacts
	latest  map[string]string // suite/arch to version
	macNew  string
	busy    map[string]hostRow // rows mid-upgrade, which a check mustn't overwrite
	wake    func()
	again   chan bool // true for a full check
	checked time.Time
}

func newHostsWatch(wake func()) *hostsWatch {
	w := &hostsWatch{facts: map[string]remoteFacts{}, latest: map[string]string{}, busy: map[string]hostRow{},
		wake: wake, again: make(chan bool, 1)}
	go w.run()
	return w
}

func (w *hostsWatch) snapshot() []hostRow {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := append([]hostRow(nil), w.rows...)
	for i, r := range out {
		if b, ok := w.busy[r.Label]; ok {
			out[i] = b
		}
	}
	return out
}

func (w *hostsWatch) recheck(full bool) {
	select {
	case w.again <- full:
	default:
	}
}

func (w *hostsWatch) run() {
	lastFull := time.Time{}
	for {
		full := time.Since(lastFull) > 30*time.Minute
		if full {
			lastFull = time.Now()
		}
		w.collect(full)
		w.wake()
		select {
		case f := <-w.again:
			if f {
				lastFull = time.Time{}
			}
		case <-time.After(60 * time.Second):
		}
	}
}

func (w *hostsWatch) collect(full bool) {
	labels := rexHosts()
	if full {
		macNew := ""
		if runtime.GOOS == "darwin" {
			macNew = latestMacBuild()
		}
		facts := map[string]remoteFacts{}
		var mu sync.Mutex
		var wg sync.WaitGroup
		for _, l := range labels {
			wg.Add(1)
			go func(l string) {
				defer wg.Done()
				if f, ok := readFacts(l); ok {
					mu.Lock()
					facts[l] = f
					mu.Unlock()
				}
			}(l)
		}
		wg.Wait()
		latest := map[string]string{}
		for _, f := range facts {
			key := f.suite + "/" + f.arch
			if f.suite != "" && f.arch != "" && latest[key] == "" {
				latest[key] = latestApt(f.suite, f.arch)
			}
		}
		w.mu.Lock()
		w.macNew, w.facts, w.latest = macNew, facts, latest
		w.mu.Unlock()
	}

	rows := make([]hostRow, len(labels)+1)
	rows[0] = w.localRow()
	var wg sync.WaitGroup
	for i, l := range labels {
		wg.Add(1)
		go func(i int, l string) {
			defer wg.Done()
			rows[i+1] = w.remoteRow(l)
		}(i, l)
	}
	wg.Wait()
	w.mu.Lock()
	w.rows, w.checked = rows, time.Now()
	w.mu.Unlock()
}

func (w *hostsWatch) localRow() hostRow {
	row := hostRow{}
	if sv, ok := readServer(""); ok {
		row.Running, row.Platform = sv.Version, prettyPlatform(sv.Platform)
	}
	if runtime.GOOS != "darwin" {
		row.Status = hsUpToDate
		if row.Running == "" {
			row.Status, row.Note = hsUnknown, "Rex's server isn't answering"
		}
		return row
	}
	short, build := plistValue(rexApp, "CFBundleShortVersionString"), plistValue(rexApp, "CFBundleVersion")
	if build == "" {
		row.Status, row.Note = hsUnknown, "Rex app not found"
		return row
	}
	row.App = fmt.Sprintf("%s (%s)", short, build)
	w.mu.Lock()
	latest := w.macNew
	w.mu.Unlock()
	row.Latest = latest
	switch {
	case latest == "":
		row.Status, row.Note = hsUnknown, "Couldn't check for updates"
	case compareVersions(build, latest) < 0:
		row.Status = hsMacUpdate
	default:
		row.Status = hsUpToDate
	}
	return row
}

func (w *hostsWatch) remoteRow(label string) hostRow {
	row := hostRow{Label: label}
	w.mu.Lock()
	f, haveFacts := w.facts[label]
	w.mu.Unlock()
	sv, ok := readServer(label)
	if !ok {
		row.Status = hsUnreachable
		// The usual reason after a restart is a server that took the socket
		// without Tailscale, and Restart can fix that.
		if nf, got := readFacts(label); got {
			f, haveFacts = nf, true
		}
		if haveFacts {
			row.Installed, row.Rexurrect = f.installed, f.rexurrect
			row.Stray = f.unit && f.stray
			row.CanRestart = row.Stray
		}
		return row
	}
	row.Running, row.Platform = sv.Version, prettyPlatform(sv.Platform)
	if out, err := rexOutput(5*time.Second, label, "api", "call", "session.list"); err == nil {
		var res struct {
			Sessions []struct {
				Label string `json:"label"`
			} `json:"sessions"`
		}
		json.Unmarshal(out, &res)
		for _, s := range res.Sessions {
			if s.Label != "" {
				row.Sessions = append(row.Sessions, s.Label)
			}
		}
	}
	if !haveFacts {
		row.Status, row.Note = hsUnknown, "No SSH access"
		return row
	}
	row.Installed, row.Rexurrect = f.installed, f.rexurrect
	row.CanUpgrade = f.sudo && f.suite != ""
	// Only restart through systemd when systemd is what runs the server;
	// otherwise a second server would start beside the one running.
	row.CanRestart = f.unit && f.active
	switch {
	case f.unit && f.stray:
		row.Stray, row.CanRestart = true, true
	case !f.unit:
		row.RestartBlocked = "No rex-user.service here to restart."
	case !f.active:
		row.RestartBlocked = "Rex here wasn't started by rex-user.service, so restarting it would start a second server."
	}
	w.mu.Lock()
	row.Latest = w.latest[f.suite+"/"+f.arch]
	w.mu.Unlock()
	switch {
	case row.Latest != "" && row.Installed != "" && compareVersions(row.Installed, row.Latest) < 0:
		row.Status = hsUpdate
	case row.Installed != "" && compareVersions(row.Running, row.Installed) < 0:
		row.Status = hsRestart
	case row.Installed == "" && row.Latest != "" && compareVersions(row.Running, row.Latest) < 0:
		row.Status = hsUpdate
	case row.Latest == "" && row.Installed == "":
		row.Status, row.Note = hsUnknown, "Couldn't check for updates"
	default:
		row.Status = hsUpToDate
	}
	return row
}

// steps is what an upgrade or restart of h will do, for the confirmation.
func (h hostRow) steps() (title string, lines []string) {
	upgrading := h.Status == hsUpdate
	title = "Restart Rex on " + h.Label + "?"
	if upgrading {
		title = "Upgrade Rex on " + h.Label + "?"
		lines = append(lines, "Install the newer rex package with apt (sudo)")
	}
	if h.Rexurrect {
		lines = append(lines, "Save your sessions with rexurrect")
	}
	if h.Stray {
		lines = append(lines, "Stop the Rex server that took the socket without Tailscale (its sessions close)")
	} else {
		lines = append(lines, "Stop any Rex server that grabs the socket without Tailscale meanwhile")
	}
	lines = append(lines, "Restart rex-user.service, the server your devices connect to")
	if len(h.Sessions) > 0 {
		warn := "Open sessions there (" + strings.Join(h.Sessions, ", ") + ") may close"
		if h.Rexurrect {
			warn += "; rexurrect saves them first"
		}
		lines = append(lines, warn)
	}
	return title, lines
}

// upgrade upgrades or restarts Rex on a host over SSH, after you confirmed.
func (w *hostsWatch) upgrade(h hostRow) {
	upgrading := h.Status == hsUpdate
	target := h.Installed
	if upgrading {
		target = h.Latest
	}
	set := func(st hostStatus, note string) {
		w.mu.Lock()
		b := h
		b.Status, b.Note = st, note
		w.busy[h.Label] = b
		w.mu.Unlock()
		w.wake()
	}
	if upgrading {
		set(hsWorking, "Upgrading…")
	} else {
		set(hsWorking, "Restarting…")
	}
	go func() {
		script := strayServers + "set -e\n"
		if upgrading {
			script += "echo '==> apt-get update'\nsudo -n apt-get update -qq\n" +
				"echo '==> apt-get install rex'\nsudo -n env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --only-upgrade rex\n"
		}
		if h.Rexurrect {
			script += "echo '==> saving sessions'\nsystemctl --user start rexurrect-autosave.service || true\n"
		}
		script += `echo '==> restarting rex-user.service'
systemctl --user stop rex-user.service
stop_stray
systemctl --user reset-failed rex-user.service 2>/dev/null || true
systemctl --user start rex-user.service
sleep 3
if ! systemctl --user is-active --quiet rex-user.service; then
    stop_stray
    systemctl --user reset-failed rex-user.service 2>/dev/null || true
    systemctl --user start rex-user.service
    sleep 3
    systemctl --user is-active --quiet rex-user.service || { echo "rex-user.service didn't start: $(journalctl --user -u rex-user.service -n 1 -o cat 2>/dev/null)" >&2; exit 1; }
fi
`
		_, errOut, err := sshRun(h.Label, script, 5*time.Minute)
		if err != nil {
			reason := strings.TrimSpace(lastLine(errOut))
			if reason == "" {
				reason = "Failed: " + err.Error()
			}
			set(hsFailed, reason)
			return
		}
		set(hsWorking, "Waiting for "+h.Label+"…")
		for i := 0; i < 30; i++ {
			time.Sleep(2 * time.Second)
			if sv, ok := readServer(h.Label); ok && (target == "" || compareVersions(sv.Version, target) >= 0) {
				w.mu.Lock()
				delete(w.busy, h.Label)
				w.mu.Unlock()
				w.recheck(true)
				return
			}
		}
		set(hsFailed, h.Label+" didn't come back on the new version")
	}()
}

// dismiss clears a failed upgrade's message.
func (w *hostsWatch) dismiss(label string) {
	w.mu.Lock()
	if b, ok := w.busy[label]; ok && b.Status == hsFailed {
		delete(w.busy, label)
	}
	w.mu.Unlock()
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// --- versions -------------------------------------------------------------------

// compareVersions compares Debian-style versions, enough for Rex's.
func compareVersions(a, b string) int {
	ta, tb := versionTokens(a), versionTokens(b)
	for i := 0; i < max(len(ta), len(tb)); i++ {
		var x, y string
		hx, hy := i < len(ta), i < len(tb)
		if hx {
			x = ta[i]
		}
		if hy {
			y = tb[i]
		}
		if hx && hy && x == y {
			continue
		}
		switch {
		case !hx:
			if y == "~" {
				return 1
			}
			return -1
		case !hy:
			if x == "~" {
				return -1
			}
			return 1
		case x == "~":
			return -1
		case y == "~":
			return 1
		}
		nx, ex := strconv.Atoi(x)
		ny, ey := strconv.Atoi(y)
		if ex == nil && ey == nil {
			if nx < ny {
				return -1
			}
			return 1
		}
		if x < y {
			return -1
		}
		return 1
	}
	return 0
}

func versionTokens(s string) []string {
	var out []string
	cur := ""
	digits := -1
	for _, c := range s {
		if c == '~' {
			if cur != "" {
				out = append(out, cur)
			}
			out, cur, digits = append(out, "~"), "", -1
			continue
		}
		if !unicode.IsLetter(c) && !unicode.IsDigit(c) {
			if cur != "" {
				out = append(out, cur)
			}
			cur, digits = "", -1
			continue
		}
		d := 0
		if unicode.IsDigit(c) {
			d = 1
		}
		if digits >= 0 && digits != d {
			out, cur = append(out, cur), ""
		}
		cur += string(c)
		digits = d
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// describeVersion is a version for people: snapshot builds, numbered by
// their build time (0.0.<seconds>-g<commit>), read as a date.
func describeVersion(v string) string {
	parts := strings.Split(v, ".")
	if len(parts) >= 3 && parts[0] == "0" && parts[1] == "0" {
		digits := strings.TrimRightFunc(strings.SplitN(parts[2], "-", 2)[0], func(r rune) bool { return !unicode.IsDigit(r) })
		if n, err := strconv.ParseInt(digits, 10, 64); err == nil && n > 1_600_000_000 {
			return time.Unix(n, 0).Format("Jan 2, 3:04 pm") + " build"
		}
	}
	if v == "" {
		return "unknown"
	}
	return v
}

// --- the card --------------------------------------------------------------------

func (h hostRow) statusText() (string, int) { // text, and 0 plain, 1 good, 2 attention, 3 bad
	switch h.Status {
	case hsChecking:
		return "Checking…", 0
	case hsUpToDate:
		return "Up to date", 1
	case hsUpdate:
		return "Update: " + describeVersion(h.Latest), 2
	case hsRestart:
		return "Installed " + describeVersion(h.Installed) + ": restart to use it", 2
	case hsMacUpdate:
		return "Build " + h.Latest + " is out: update from Rex's menu", 2
	case hsUnreachable:
		if h.Stray {
			return "Off the tailnet: a server without Tailscale holds the socket", 3
		}
		return "Not answering", 3
	case hsWorking:
		return h.Note, 0
	case hsFailed:
		return h.Note, 3
	}
	return h.Note, 0
}

func (h hostRow) action() string {
	if h.Status == hsWorking {
		return ""
	}
	switch {
	case h.Status == hsUpdate && h.CanUpgrade:
		return "Upgrade"
	case h.Status == hsRestart && h.CanRestart, h.Stray && h.CanRestart:
		return "Restart"
	case h.Status == hsFailed:
		return "OK"
	}
	return ""
}

func (v *activityView) drawHosts(p avPaint, c avColors, x0, x1, y float64) float64 {
	r := p.r
	rows := v.hosts
	if len(rows) == 0 {
		return y
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Label == "" && rows[j].Label != "" })
	var folded bool
	if y, folded = v.section(p, foldHosts, "REX HOSTS", 0, r.pal.gray, x0, x1, y); folded {
		return y
	}
	ix0, ix1 := x0+avPad, x1-avPad
	// Measure first, for the card behind.
	heights := make([]float64, len(rows))
	total := 0.0
	for i, h := range rows {
		heights[i] = 58
		if v.confirm == h.Label && h.Label != "" {
			_, lines := h.steps()
			heights[i] += 34 + float64(len(lines))*18 + avBtnH + 14
		}
		total += heights[i]
	}
	p.box(x0, y, x1, y+total, avRadius, c.card)
	p.outline(x0, y, x1, y+total, avRadius, c.cardLine)
	ry := y
	for i, h := range rows {
		if i > 0 {
			p.rect(ix0, ry, ix1, ry+1, c.cardLine)
		}
		text, tone := h.statusText()
		col := map[int]color.RGBA{0: r.pal.gray, 1: c.good, 2: c.needs, 3: c.warn}[tone]
		act := h.action()
		bw := 0.0
		if act != "" {
			bw = r.width(r.clean(act, true), 13, true) + 28
			bx1 := ix1
			bx0 := bx1 - bw
			by0 := ry + 13
			fill, ink := c.work, rgb(255, 255, 255)
			if act == "OK" {
				fill, ink = c.well, r.pal.text
			}
			p.box(bx0, by0, bx1, by0+32, 9, fill)
			p.text(act, bx0+14, by0+21, 13, true, ink, 0)
			if p.on() {
				v.hits = append(v.hits, avHit{bx0, by0, bx1, by0 + 32, "host:" + act + ":" + h.Label})
			}
		}
		maxW := ix1 - ix0 - bw - 10
		nameW := p.text(h.name(), ix0, ry+24, 14.5, true, r.pal.text, maxW)
		ver := describeVersion(h.Running)
		if h.App != "" {
			ver = "Rex " + h.App
		}
		if h.Status == hsUnreachable && h.Running == "" {
			ver = ""
		}
		if ver != "" {
			p.text(ver, ix0+nameW+8, ry+24, 12, false, r.pal.faint, maxW-nameW-8)
		}
		p.text(text, ix0, ry+43, 12, tone >= 2, col, maxW)

		if v.confirm == h.Label && h.Label != "" {
			title, lines := h.steps()
			cy := ry + 58
			p.text(title, ix0, cy+16, 14, true, r.pal.text, ix1-ix0)
			cy += 30
			for _, l := range lines {
				p.text("• "+l, ix0, cy+4, 12, false, r.pal.gray, ix1-ix0)
				cy += 18
			}
			cy += 8
			half := (ix1 - ix0 - 8) / 2
			p.box(ix0, cy, ix0+half, cy+avBtnH, 10, c.well)
			lw := r.width("Cancel", 14, false)
			p.text("Cancel", ix0+half/2-lw/2, cy+avBtnH/2+5, 14, false, r.pal.text, 0)
			verb := "Restart"
			if h.Status == hsUpdate {
				verb = "Upgrade"
			}
			p.box(ix1-half, cy, ix1, cy+avBtnH, 10, c.needsFill)
			lw = r.width(r.clean(verb, true), 14, true)
			p.text(verb, ix1-half/2-lw/2, cy+avBtnH/2+5, 14, true, c.needsInk, 0)
			if p.on() {
				v.hits = append(v.hits, avHit{ix0, cy, ix0 + half, cy + avBtnH, "host:cancel:" + h.Label})
				v.hits = append(v.hits, avHit{ix1 - half, cy, ix1, cy + avBtnH, "host:go:" + h.Label})
			}
		}
		ry += heights[i]
	}
	return y + total
}
