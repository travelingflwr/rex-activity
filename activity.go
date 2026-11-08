package main

// The Activity pane: what every agent is up to, in the order it matters to
// you. Agents that need you come first, then the ones working, with how
// full their context is, then the ones that finished. The plan's usage sits
// above them, since every agent on the account shares it. A wide pane puts
// what needs you in a column of its own beside the rest; a narrow one
// stacks it all, phone-style. It draws in the pane's own colours, so it
// follows the Rex theme, light or dark.

import (
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"os"
	"strings"
	"time"
)

// Layout, in points.
const (
	avColMax  = 540.0 // a column's widest
	avWide    = 640.0 // from this pane width, two columns
	avColGap  = 20.0
	avTabH    = 34.0  // a row of tabs
	avStacked = 420.0 // under this column width the tabs don't read: the sections take their place
	avPad     = 14.0  // inside a card
	avGap     = 10.0  // between cards
	avBtnH    = 36.0
	avRadius  = 14.0
	avScrollN = 48.0
)

type avTab int

const (
	tabAll avTab = iota
	tabNeeds
	tabWorking
	tabDone
)

// avHit is a place on the pane that does something when clicked.
type avHit struct {
	x0, y0, x1, y1 float64
	action         string // tab:N, fold:N or open:KEY
}

type activityView struct {
	agents  []Agent
	plan    planUsage
	hasPlan bool
	tab     avTab
	scroll  float64
	hits    []avHit
	msg     string
	hosts   int // Rex servers with agents on them
	folded  map[avTab]bool
	stacked bool // the pane's too narrow for a row of tabs: they stack in the list
}

// shownTab is the tab in effect.
func (v *activityView) shownTab() avTab { return v.tab }

func (v *activityView) sections() (needs, working, done []Agent) {
	for _, a := range v.agents {
		switch a.State {
		case stWaiting:
			needs = append(needs, a)
		case stWorking:
			working = append(working, a)
		default:
			done = append(done, a)
		}
	}
	return
}

func (v *activityView) set(agents []Agent) {
	v.agents = agents
	hosts := map[string]bool{}
	for _, a := range agents {
		hosts[a.Host] = true
	}
	v.hosts = len(hosts)
	v.plan, v.hasPlan = loadPlan()
}

func runActivity(args []string) error {
	s, done, err := openScreen("")
	if err != nil {
		return err
	}
	defer done()
	sc := newScanner()
	v := &activityView{}
	v.set(sc.agents())

	poll := time.NewTicker(400 * time.Millisecond)
	defer poll.Stop()
	slow := time.NewTicker(30 * time.Second)
	defer slow.Stop()
	msgUntil := time.Time{}
	lastKey := ""
	self, _ := os.Executable()
	built := fileStamp(self)
	checked := time.Now()

	draw := func() {
		key := fmt.Sprint(s.ws, s.pal, v.drawKey())
		if key == lastKey {
			return
		}
		lastKey = key
		s.begin()
		s.background()
		s.show("activity", key, 1, s.ws.rows, 0, func() *image.RGBA {
			img := s.canvas(s.heightPt())
			v.draw(s.renderer(), img, s.widthPt(), s.heightPt())
			return img
		})
		s.end()
	}
	say := func(m string) { v.msg, msgUntil = m, time.Now().Add(3*time.Second) }
	act := func(action string) {
		kind, id, _ := strings.Cut(action, ":")
		switch kind {
		case "fold":
			var n int
			fmt.Sscan(id, &n)
			if v.folded == nil {
				v.folded = map[avTab]bool{}
			}
			v.folded[avTab(n)] = !v.folded[avTab(n)]
		case "tab":
			var n int
			fmt.Sscan(id, &n)
			if avTab(n) == v.tab {
				n = int(tabAll)
			}
			v.tab, v.scroll = avTab(n), 0
		case "open":
			for _, a := range v.agents {
				if a.key() == id {
					if err := focus(a); err != nil {
						say("Couldn't find its pane")
					}
				}
			}
		}
	}
	draw()

	for {
		select {
		case <-s.quit:
			return nil
		case <-s.winch:
			s.resize()
			lastKey = ""
			draw()
		case <-s.snapSig:
			s.saveSnapshot("activity")
		case <-sc.changed:
			v.set(sc.agents())
			draw()
		case <-poll.C:
			s.recheck()
			// A new build at our path: start again on it, in place.
			if time.Since(checked) > 2*time.Second {
				checked = time.Now()
				if st := fileStamp(self); st != "" && built != "" && st != built {
					return errRestart
				}
			}
			if v.msg != "" && time.Now().After(msgUntil) {
				v.msg = ""
				draw()
			}
		case <-slow.C:
			v.plan, v.hasPlan = loadPlan()
			lastKey = "" // the ages move on
			draw()
		case ev, open := <-s.events:
			if !open {
				return nil
			}
			if s.event(ev) {
				lastKey = ""
				draw()
				continue
			}
			if ev.click != nil {
				x, y := s.clickPt(ev.click)
				for i := len(v.hits) - 1; i >= 0; i-- {
					h := v.hits[i]
					if x >= h.x0 && x < h.x1 && y >= h.y0 && y < h.y1 {
						act(h.action)
						draw()
						break
					}
				}
				continue
			}
			if ev.scroll != 0 {
				switch ev.scroll {
				case pageDown:
					v.scroll += s.heightPt() * 0.8
				case pageUp:
					v.scroll -= s.heightPt() * 0.8
				case scrollBottom:
					v.scroll = 1e9
				case scrollTop:
					v.scroll = 0
				case wheelDown, lineDown:
					v.scroll += avScrollN
				default:
					v.scroll -= avScrollN
				}
				draw()
				continue
			}
			k := ev.key
			switch {
			case k.is('q', 0), k.is(keyEscape, 0), k.is('c', modCtrl):
				return nil
			case k.is('j', 0), k.is(keyDown, 0):
				v.scroll += avScrollN
			case k.is('k', 0), k.is(keyUp, 0):
				v.scroll -= avScrollN
			case k.is('g', 0), k.is(keyHome, 0):
				v.scroll = 0
			case k.is('1', 0), k.is('2', 0), k.is('3', 0), k.is('4', 0):
				v.tab, v.scroll = avTab(k.code-'1'), 0
			case k.is(keyEnter, 0), k.is('o', 0):
				if len(v.agents) > 0 {
					act("open:" + v.agents[0].key())
				}
			default:
				continue
			}
			draw()
		}
	}
}

func (v *activityView) drawKey() string {
	var b strings.Builder
	fmt.Fprint(&b, v.tab, v.folded, v.scroll, v.msg, v.hasPlan, v.plan.Updated.Unix())
	for _, a := range v.agents {
		fmt.Fprint(&b, a.key(), a.State, a.Msg, a.Title, a.Since.Unix(), a.Ctx.Tokens, ";")
	}
	return b.String()
}

// errRestart asks main to run the new build in this pane.
var errRestart = errors.New("restart")

// fileStamp tells one build at a path from another.
func fileStamp(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fmt.Sprint(fi.Size(), fi.ModTime().UnixNano())
}

// --- drawing ---------------------------------------------------------------

// avPaint draws on img, or only measures when img is nil.
type avPaint struct {
	r   *renderer
	img *image.RGBA
}

func (p avPaint) on() bool { return p.img != nil }

func (p avPaint) text(s string, x, y, size float64, bold bool, col color.RGBA, maxW float64) float64 {
	if p.img == nil {
		return p.r.width(p.r.fit(p.r.clean(s, bold), size, bold, maxW), size, bold)
	}
	return p.r.text(p.img, s, x, y, size, bold, col, maxW)
}

func (p avPaint) right(s string, x, y, size float64, bold bool, col color.RGBA) float64 {
	if p.img == nil {
		return p.r.width(p.r.clean(s, bold), size, bold)
	}
	return p.r.textRight(p.img, s, x, y, size, bold, col)
}

func (p avPaint) box(x0, y0, x1, y1, rad float64, col color.RGBA) {
	if p.img != nil {
		p.r.fillRoundRect(p.img, x0, y0, x1, y1, rad, col)
	}
}

func (p avPaint) rect(x0, y0, x1, y1 float64, col color.RGBA) {
	if p.img != nil {
		p.r.fillRect(p.img, x0, y0, x1, y1, col)
	}
}

func (p avPaint) dot(cx, cy, rad float64, col color.RGBA) {
	if p.img != nil {
		p.r.fillCircle(p.img, cx, cy, rad, col)
	}
}

// outline is a rounded box's border, drawn as a ring.
func (p avPaint) outline(x0, y0, x1, y1, rad float64, col color.RGBA) {
	if p.img == nil {
		return
	}
	pb := p.r.path(p.img)
	pb.poly(roundRect(x0, y0, x1, y1, rad))
	pb.poly(reversed(roundRect(x0+1, y0+1, x1-1, y1-1, rad-1)))
	pb.fill(col)
}

// avColors are the view's colours, all from the pane's theme.
type avColors struct {
	card, cardLine, well, track, needs, needsFill, needsInk, needsLine, work, good, warn color.RGBA
}

func themeColors(r *renderer) avColors {
	amber := r.amber()
	return avColors{
		card:      r.tinted(r.pal.text, 0.05),
		cardLine:  r.tinted(r.pal.text, 0.10),
		well:      r.tinted(r.pal.text, 0.06),
		track:     r.tinted(r.pal.text, 0.12),
		needs:     amber,
		needsFill: r.themed(rgb(161, 98, 0), amber),
		needsInk:  r.themed(rgb(255, 255, 255), rgb(26, 20, 6)),
		needsLine: r.tinted(amber, 0.45),
		work:      r.pal.accent,
		good:      r.green(),
		warn:      r.themed(rgb(196, 90, 20), rgb(242, 145, 90)),
	}
}

// avColumn is one column of the list and what goes in it.
type avColumn struct {
	x0, x1             float64
	plan               bool
	needs, work, fresh bool // which sections
}

// columns lays the list out for the pane's width: two columns when it's
// wide and showing everything, else one.
func (v *activityView) columns(w float64) (cols []avColumn, x0, x1 float64) {
	tab := v.shownTab()
	if tab == tabAll && w >= avWide {
		colW := min((w-32-avColGap)/2, avColMax)
		x0 = (w - 2*colW - avColGap) / 2
		x1 = w - x0
		return []avColumn{
			{x0: x0, x1: x0 + colW, needs: true},
			{x0: x1 - colW, x1: x1, plan: true, work: true, fresh: true},
		}, x0, x1
	}
	colW := min(w-24, avColMax)
	x0 = (w - colW) / 2
	x1 = x0 + colW
	one := avColumn{x0: x0, x1: x1}
	switch tab {
	case tabAll:
		one.plan, one.needs, one.work, one.fresh = true, true, true, true
	case tabNeeds:
		one.needs = true
	case tabWorking:
		one.work = true
	case tabDone:
		one.fresh = true
	}
	return []avColumn{one}, x0, x1
}

// headHeight is the pinned title, and the tabs when they show.
func (v *activityView) headHeight() float64 {
	if v.stacked {
		return 66
	}
	return 104
}

func (v *activityView) draw(r *renderer, img *image.RGBA, w, h float64) {
	v.hits = v.hits[:0]
	c := themeColors(r)
	needs, working, done := v.sections()
	v.stacked = min(w-24, avColMax) < avStacked
	cols, x0, x1 := v.columns(w)
	avHeadH := v.headHeight()

	// The list, scrolled, under the pinned header.
	tallest := 0.0
	for _, col := range cols {
		tallest = max(tallest, v.drawColumn(avPaint{r: r}, c, col, needs, working, done, 0, len(cols) > 1))
	}
	v.scroll = max(0, min(v.scroll, tallest-(h-avHeadH)+16))
	for _, col := range cols {
		v.drawColumn(avPaint{r: r, img: img}, c, col, needs, working, done, avHeadH-v.scroll, len(cols) > 1)
	}
	// Nothing of the list shows behind the header, and its clicks there
	// don't count.
	draw.Draw(img, image.Rect(0, 0, img.Bounds().Dx(), int(avHeadH*r.scale)), image.Transparent, image.Point{}, draw.Src)
	if !r.clear {
		r.fillRect(img, 0, 0, w, avHeadH, r.pal.bg)
	}
	kept := v.hits[:0]
	for _, ht := range v.hits {
		if ht.y0 >= avHeadH-1 {
			kept = append(kept, ht)
		}
	}
	v.hits = kept
	v.drawHeader(avPaint{r: r, img: img}, c, len(needs), len(working), len(done), x0, x1)
}

func (v *activityView) drawHeader(p avPaint, c avColors, nNeeds, nWorking, nDone int, x0, x1 float64) {
	r := p.r
	sub := fmt.Sprintf("%d agent%s", len(v.agents), plural(len(v.agents)))
	if v.hosts > 1 {
		sub = fmt.Sprintf("%d hosts · ", v.hosts) + sub
	}
	if v.msg != "" {
		sub = v.msg
	}
	p.text(sub, x0+2, 20, 12, false, r.pal.gray, 0)
	p.text("Activity", x0, 46, 24, true, r.pal.text, 0)

	if v.stacked {
		return
	}
	ty0, ty1 := 58.0, 58.0+avTabH
	p.box(x0, ty0, x1, ty1, 10, c.card)
	tabs := []struct {
		label string
		n     int
		fill  color.RGBA
	}{
		{"All", 0, c.work},
		{"Needs you", nNeeds, c.needsFill},
		{"Working", nWorking, c.work},
		{"Done", nDone, c.work},
	}
	tw := (x1 - x0 - 8) / float64(len(tabs))
	for i, t := range tabs {
		bx0 := x0 + 4 + float64(i)*tw
		bx1 := bx0 + tw
		label := t.label
		if i > 0 && t.n > 0 {
			label = fmt.Sprintf("%s %d", t.label, t.n)
		}
		sel := avTab(i) == v.tab
		hot := i == int(tabNeeds) && t.n > 0
		col := r.pal.gray
		switch {
		case sel:
			p.box(bx0, ty0+4, bx1, ty1-4, 8, t.fill)
			col = rgb(255, 255, 255)
			if i == int(tabNeeds) {
				col = c.needsInk
			}
		case hot:
			col = c.needs
		}
		bold := sel || hot
		lw := r.width(r.fit(r.clean(label, bold), 13, bold, tw-8), 13, bold)
		p.text(label, (bx0+bx1)/2-lw/2, (ty0+ty1)/2+4.7, 13, bold, col, tw-8)
		v.hits = append(v.hits, avHit{bx0, ty0, bx1, ty1, fmt.Sprintf("tab:%d", i)})
	}
}

// drawColumn draws one column from y and returns how tall it is.
func (v *activityView) drawColumn(p avPaint, c avColors, col avColumn, needs, working, done []Agent, y float64, split bool) float64 {
	start := y
	y += 10
	r := p.r
	x0, x1 := col.x0, col.x1
	if v.stacked {
		y = v.tabList(p, c, x0, x1, y, len(needs), len(working), len(done)) + 18
	}
	if col.plan && v.hasPlan {
		y = v.drawPlan(p, c, x0, x1, y) + 18
	}
	shown := false
	if col.needs && (len(needs) > 0 || split) {
		shown = true
		var folded bool
		y, folded = v.section(p, tabNeeds, "NEEDS YOU", len(needs), c.needs, x0, x1, y)
		if folded {
			needs = nil
		} else if len(needs) == 0 {
			p.box(x0, y, x1, y+52, avRadius, c.card)
			p.text("Nothing needs you right now", x0+avPad, y+31, 13.5, false, r.pal.gray, x1-x0-2*avPad)
			y += 52 + avGap
		}
		for _, a := range needs {
			y = v.needsCard(p, c, a, x0, x1, y) + avGap
		}
		y += 8
	}
	if col.work && len(working) > 0 {
		shown = true
		var folded bool
		if y, folded = v.section(p, tabWorking, "WORKING", len(working), c.work, x0, x1, y); !folded {
			y = v.rows(p, c, working, x0, x1, y, false) + 18
		}
	}
	if col.fresh && len(done) > 0 {
		shown = true
		var folded bool
		if y, folded = v.section(p, tabDone, "DONE", len(done), c.good, x0, x1, y); !folded {
			y = v.rows(p, c, done, x0, x1, y, true) + 18
		}
	}
	if !shown && !(col.plan && v.hasPlan) {
		msg := map[avTab]string{
			tabAll:     "No agents running",
			tabNeeds:   "Nothing needs you",
			tabWorking: "Nothing working",
			tabDone:    "Nothing finished yet",
		}[v.shownTab()]
		y += 30
		p.text(msg, x0+2, y, 15, true, r.pal.gray, 0)
		if v.shownTab() == tabAll {
			y += 22
			p.text("Start Claude Code or Codex in a Rex pane and it shows up here.", x0+2, y, 12.5, false, r.pal.faint, x1-x0)
		}
		y += 20
	}
	return y - start
}

// tabList is the tabs stacked one per row, for a narrow pane: each with its
// count, the chosen one filled.
func (v *activityView) tabList(p avPaint, c avColors, x0, x1, y float64, nNeeds, nWorking, nDone int) float64 {
	r := p.r
	tabs := []struct {
		label string
		n     int
		dot   color.RGBA
		fill  color.RGBA
	}{
		{"All", nNeeds + nWorking + nDone, r.pal.gray, c.work},
		{"Needs you", nNeeds, c.needs, c.needsFill},
		{"Working", nWorking, c.work, c.work},
		{"Done", nDone, c.good, c.work},
	}
	const rowH = 40.0
	h := rowH*float64(len(tabs)) + 8
	p.box(x0, y, x1, y+h, avRadius, c.card)
	p.outline(x0, y, x1, y+h, avRadius, c.cardLine)
	for i, t := range tabs {
		ry0 := y + 4 + float64(i)*rowH
		ry1 := ry0 + rowH
		sel := avTab(i) == v.tab
		ink, sub := r.pal.text, r.pal.gray
		if sel {
			p.box(x0+4, ry0+2, x1-4, ry1-2, 10, t.fill)
			ink = rgb(255, 255, 255)
			if i == int(tabNeeds) {
				ink = c.needsInk
			}
			sub = ink
		} else if i > 0 {
			p.dot(x0+avPad+4, (ry0+ry1)/2, 4, t.dot)
		}
		hot := i == int(tabNeeds) && t.n > 0 && !sel
		lx := x0 + avPad + 16
		if i == 0 && !sel {
			lx = x0 + avPad
		}
		if sel {
			lx = x0 + avPad
		}
		col := ink
		if hot {
			col = c.needs
		}
		p.text(t.label, lx, (ry0+ry1)/2+5, 14.5, sel || hot, col, x1-lx-50)
		if t.n > 0 || i == 0 {
			p.right(fmt.Sprint(t.n), x1-avPad, (ry0+ry1)/2+5, 13.5, sel || hot, sub)
		}
		if i > 0 && !sel && avTab(i-1) != v.tab {
			p.rect(x0+avPad, ry0, x1-avPad, ry0+1, c.cardLine)
		}
		if p.on() {
			v.hits = append(v.hits, avHit{x0, ry0, x1, ry1, fmt.Sprintf("tab:%d", i)})
		}
	}
	return y + h
}

// section draws a section's heading, which folds it away or opens it
// again, and reports whether it's folded.
func (v *activityView) section(p avPaint, which avTab, label string, n int, col color.RGBA, x0, x1, y float64) (float64, bool) {
	r := p.r
	folded := v.folded[which]
	p.dot(x0+5, y+10, 4, col)
	lw := p.text(label, x0+16, y+14.5, 11.5, true, col, 0)
	if n > 0 {
		p.text(fmt.Sprint(n), x0+16+lw+8, y+14.5, 11.5, true, r.pal.gray, 0)
	}
	// A chevron: pointing down while open, right while folded.
	if p.on() {
		cx, cy := x1-10, y+10
		pts := [][2]float64{{cx - 4, cy - 2}, {cx + 4, cy - 2}, {cx, cy + 3}}
		if folded {
			pts = [][2]float64{{cx - 2, cy - 4}, {cx + 3, cy}, {cx - 2, cy + 4}}
		}
		pb := r.path(p.img)
		pb.poly(pts)
		pb.fill(r.pal.gray)
		v.hits = append(v.hits, avHit{x0, y - 6, x1, y + 30, fmt.Sprintf("fold:%d", which)})
	}
	if folded {
		return y + 34, true
	}
	return y + 26, false
}

func (v *activityView) drawPlan(p avPaint, c avColors, x0, x1, y float64) float64 {
	r := p.r
	type row struct {
		label, note string
		w           *planWindow
	}
	var rows []row
	add := func(label string, w *planWindow) {
		if w == nil {
			return
		}
		note := fmt.Sprintf("%.0f%%", w.Used)
		if !w.ResetAt.IsZero() {
			note += " · resets " + resetText(r.now, w.ResetAt)
		}
		rows = append(rows, row{label, note, w})
	}
	add("Claude · 5-hour", v.plan.FiveHour)
	add("Claude · weekly", v.plan.Weekly)
	ix0, ix1 := x0+avPad, x1-avPad
	inner := ix1 - ix0
	// Too narrow for a label and its numbers side by side: the numbers go
	// on a line of their own.
	stack := false
	for _, rw := range rows {
		if r.width(r.clean(rw.label, false), 12.5, false)+r.width(r.clean(rw.note, false), 12, false)+12 > inner {
			stack = true
		}
	}
	rowH := 38.0
	if stack {
		rowH = 54
	}
	h := avPad*2 + 18 + float64(len(rows))*rowH
	p.box(x0, y, x1, y+h, avRadius, c.card)
	p.outline(x0, y, x1, y+h, avRadius, c.cardLine)
	ty := y + avPad + 12
	tw := p.text("Plan usage", ix0, ty, 13, true, r.pal.text, 0)
	if sub := "shared by every agent"; tw+r.width(sub, 11.5, false)+12 <= inner {
		p.right(sub, ix1, ty, 11.5, false, r.pal.faint)
	}
	ry := ty + 10
	for _, rw := range rows {
		ry += 18
		col, bar := r.pal.gray, c.work
		if rw.w.Used >= 80 {
			col, bar = c.warn, c.warn
		}
		p.text(rw.label, ix0, ry, 12.5, false, r.pal.text, inner)
		if stack {
			ry += 16
			p.text(rw.note, ix0, ry, 12, false, col, inner)
		} else {
			p.right(rw.note, ix1, ry, 12, false, col)
		}
		by := ry + 8
		p.box(ix0, by, ix1, by+6, 3, c.track)
		p.box(ix0, by, ix0+max(6, inner*min(1, rw.w.Used/100)), by+6, 3, bar)
		ry += 20
	}
	return y + h
}

func resetText(now, t time.Time) string {
	if t.Before(now) {
		return "now"
	}
	if t.Sub(now) < 20*time.Hour {
		return strings.ToLower(t.Format("3:04 pm"))
	}
	return t.Format("Mon")
}

func hostName(a Agent) string {
	if a.Host == "" {
		return "this machine"
	}
	return a.Host
}

func meta(a Agent) string {
	return a.Kind + " · " + a.Project + " · " + hostName(a)
}

func agoText(now, t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t)
	switch {
	case d < 45*time.Second:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()+0.5))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return t.Format("Jan 2")
}

func badge(kind string) string {
	if kind == "Codex" {
		return "Cx"
	}
	return "Cl"
}

func (v *activityView) needsCard(p avPaint, c avColors, a Agent, x0, x1, y float64) float64 {
	r := p.r
	if p.on() {
		h := v.needsCard(avPaint{r: r}, c, a, x0, x1, y) - y
		p.box(x0, y, x1, y+h, avRadius, c.card)
		p.outline(x0, y, x1, y+h, avRadius, c.needsLine)
		v.hits = append(v.hits, avHit{x0, y, x1, y + h, "open:" + a.key()})
	}
	ix0, ix1 := x0+avPad, x1-avPad

	// The badge, the task and where it runs.
	p.box(ix0, y+avPad, ix0+32, y+avPad+32, 9, r.tinted(c.needs, 0.18))
	p.text(badge(a.Kind), ix0+8, y+avPad+21, 12.5, true, c.needs, 0)
	tx := ix0 + 44
	aw := p.right(agoText(r.now, a.Since), ix1, y+avPad+13, 11.5, false, r.pal.faint)
	p.text(a.Title, tx, y+avPad+13, 15, true, r.pal.text, ix1-tx-aw-10)
	p.text(meta(a), tx, y+avPad+31, 12, false, r.pal.gray, ix1-tx)
	cy := y + avPad + 32 + 12

	// What it's waiting on, as the agent told Rex.
	what := a.Msg
	if what == "" {
		what = "Waiting for you in its tab"
	}
	lines := r.wrap(what, 13.5, false, ix1-ix0-24)
	if len(lines) > 3 {
		lines = append(lines[:3:3], "…")
	}
	wellH := 26 + float64(len(lines))*18
	p.box(ix0, cy, ix1, cy+wellH, 10, c.well)
	p.text("Waiting on", ix0+12, cy+17, 11.5, false, r.pal.gray, 0)
	for i, l := range lines {
		if p.on() {
			r.drawRaw(p.img, l, ix0+12, cy+36+float64(i)*18, 13.5, false, r.pal.text)
		}
	}
	cy += wellH + 12

	// The button.
	p.box(ix0, cy, ix1, cy+avBtnH, 10, c.needsFill)
	label := "Open its tab"
	lw := r.width(r.clean(label, true), 14, true)
	p.text(label, (ix0+ix1)/2-lw/2, cy+avBtnH/2+5, 14, true, c.needsInk, 0)
	cy += avBtnH + 12

	if a.HasCtx {
		cy = v.meter(p, c, a.Ctx, ix0, ix1, cy+4, "Context") + 4
	}
	return cy + avPad - 8
}

// meter draws a context bar with its label and numbers, baseline at y.
func (v *activityView) meter(p avPaint, c avColors, use contextUse, x0, x1, y float64, label string) float64 {
	r := p.r
	frac := use.frac()
	note := fmt.Sprintf("%s / %s", kTokens(use.Tokens), kTokens(use.Window))
	col, bar := r.pal.gray, c.good
	if frac >= 0.85 {
		note = fmt.Sprintf("%.0f%% · compacts soon", frac*100)
		col, bar = c.warn, c.warn
	}
	lx := x0
	if label != "" {
		lx += p.text(label, x0, y, 11.5, false, r.pal.gray, 0) + 10
	}
	nw := p.right(note, x1, y, 11.5, false, col)
	bx0, bx1 := lx, x1-nw-10
	if bx1-bx0 > 24 {
		p.box(bx0, y-6, bx1, y-2, 2, c.track)
		p.box(bx0, y-6, bx0+max(4, (bx1-bx0)*frac), y-2, 2, bar)
	}
	return y + 6
}

func kTokens(n int) string {
	switch {
	case n >= 1_000_000 && n%1_000_000 == 0:
		return fmt.Sprintf("%dM", n/1_000_000)
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%dk", (n+500)/1000)
	}
	return fmt.Sprint(n)
}

// rows draws agents as a grouped list in one card.
func (v *activityView) rows(p avPaint, c avColors, agents []Agent, x0, x1, y float64, finished bool) float64 {
	r := p.r
	rowH := 58.0
	if finished {
		rowH = 52
	}
	h := rowH * float64(len(agents))
	p.box(x0, y, x1, y+h, avRadius, c.card)
	p.outline(x0, y, x1, y+h, avRadius, c.cardLine)
	ix0, ix1 := x0+avPad, x1-avPad
	for i, a := range agents {
		ry := y + float64(i)*rowH
		if i > 0 {
			p.rect(ix0, ry, ix1, ry+1, c.cardLine)
		}
		if p.on() {
			v.hits = append(v.hits, avHit{x0, ry, x1, ry + rowH, "open:" + a.key()})
		}
		if finished {
			if p.on() {
				r.check(p.img, ix0+7, ry+rowH/2, 12, c.good)
			}
			tx := ix0 + 24
			note := meta(a)
			if ago := agoText(r.now, a.Since); ago == "now" {
				note += " · just now"
			} else if ago != "" {
				note += " · " + ago + " ago"
			}
			p.text(a.Title, tx, ry+22, 14.5, true, r.pal.text, ix1-tx)
			p.text(note, tx, ry+39, 12, false, r.pal.gray, ix1-tx)
			continue
		}
		aw := p.right(agoText(r.now, a.Since), ix1, ry+22, 11.5, false, r.pal.faint)
		p.text(a.Title, ix0, ry+22, 14.5, true, r.pal.text, ix1-ix0-aw-10)
		line := meta(a)
		if a.Msg != "" {
			line = a.Msg
		}
		if a.HasCtx {
			mw := min(p.text(line, ix0, ry+42, 12, false, r.pal.gray, (ix1-ix0)*0.55), (ix1-ix0)*0.55)
			v.meter(p, c, a.Ctx, ix0+mw+10, ix1, ry+42, "")
		} else {
			p.text(line, ix0, ry+42, 12, false, r.pal.gray, ix1-ix0)
		}
	}
	return y + h
}
