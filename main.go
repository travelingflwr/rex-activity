// rex-activity: every coding agent running in Rex, at a glance, in a pane.
package main

import (
	"flag"
	"fmt"
	"image/png"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

const usage = `rex-activity: every coding agent in Rex, at a glance

  rex-activity                         run Activity in this pane
  rex-activity show [--below] [--ratio PCT]
                                       open it in a pane beside yours
  rex-activity statusline [COMMAND...] for Claude Code's status line: notes context
                                       and plan usage, then runs COMMAND, your own
  rex-activity render -o FILE [--theme light|dark] [--cols N] [--rows N]
                                       draw a made-up Activity to a PNG

Environment: REX_ACTIVITY_THEME=light|dark pins the colours instead of
following Rex.
`

func main() {
	args := os.Args[1:]
	var err error
	cmd := ""
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "", "run":
		err = runActivity(args)
	case "show":
		err = runShow(args)
	case "statusline":
		err = runStatusline(args)
	case "render":
		err = runRender(args)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "rex-activity: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err == errRestart {
		if self, e := os.Executable(); e == nil {
			time.Sleep(300 * time.Millisecond) // let the new build finish landing
			err = syscall.Exec(self, os.Args, os.Environ())
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "rex-activity:", strings.TrimSpace(err.Error()))
		os.Exit(1)
	}
}

func runShow(args []string) error {
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	below := fs.Bool("below", false, "split downward instead of to the right")
	ratio := fs.Int("ratio", 40, "Activity's share of the pane, in percent")
	if err := fs.Parse(args); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	split := "right"
	if *below {
		split = "below"
	}
	argv := []string{"split", "--focus=false", "--split", split, "--ratio", fmt.Sprint(*ratio), "--shell=none", "--label", "Activity"}
	if b := os.Getenv("REX_BLOCK"); b != "" {
		argv = append(argv, "-b", b)
	}
	c := exec.Command(rexBin(), append(argv, "--", self)...)
	c.Stderr = os.Stderr
	_, err = c.Output()
	return err
}

func runRender(args []string) error {
	fs := flag.NewFlagSet("render", flag.ContinueOnError)
	out := fs.String("o", "activity.png", "where to write the PNG")
	theme := fs.String("theme", "dark", "light or dark")
	cols := fs.Int("cols", 46, "pane width, in cells")
	rows := fs.Int("rows", 60, "pane height, in cells")
	tab := fs.Int("tab", 0, "the tab to show: 0 all, 1 needs you, 2 working, 3 done")
	if err := fs.Parse(args); err != nil {
		return err
	}
	fonts, err := loadFonts()
	if err != nil {
		return err
	}
	mono, err := loadMono()
	if err != nil {
		return err
	}
	s := &screen{ws: winSize{rows: *rows, cols: *cols, xpix: *cols * 16, ypix: *rows * 34}, scale: 2,
		theme: *theme, fonts: fonts, mono: mono}
	s.pal = s.colors.palette(*theme)
	r := s.renderer()
	img := s.canvas(s.heightPt())
	r.paint(img)
	v := demoActivity(r.now)
	v.tab = avTab(*tab)
	v.draw(r, img, s.widthPt(), s.heightPt())
	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

// demoActivity is made-up agents, for render.
func demoActivity(now time.Time) *activityView {
	agents := []Agent{
		{Block: "1", Kind: "Claude Code", Project: "trailhead", Host: "build-box", State: stWaiting,
			Msg: "Run: go test ./internal/route/... -run TestClosedTrail", Title: "Start Saturday's route from the lake lot",
			Since: now.Add(-2 * time.Minute), Ctx: contextUse{108_000, 200_000}, HasCtx: true},
		{Block: "2", Kind: "Codex", Project: "fieldnotes", State: stWaiting,
			Msg: "Which branch should the pull request go to, main or release-2.4?", Title: "Update the install guide",
			Since: now.Add(-9 * time.Minute)},
		{Block: "3", Kind: "Claude Code", Project: "heypad", State: stWorking, Msg: "Editing render.go",
			Title: "Give the reader a quieter quote style", Since: now.Add(-14 * time.Minute), Ctx: contextUse{182_000, 200_000}, HasCtx: true},
		{Block: "4", Kind: "Claude Code", Project: "garden-club", Host: "build-box", State: stWorking, Msg: "Reading seeds.csv",
			Title: "Dedupe the seed swap list", Since: now.Add(-6 * time.Minute), Ctx: contextUse{74_000, 200_000}, HasCtx: true},
		{Block: "5", Kind: "Codex", Project: "trailhead", Host: "lab-server", State: stWorking, Msg: "Running go test",
			Title: "Profile the slow map startup", Since: now.Add(-1 * time.Minute), Ctx: contextUse{31_000, 272_000}, HasCtx: true},
		{Block: "6", Kind: "Claude Code", Project: "fieldnotes", State: stIdle, Title: "Write the release notes",
			Since: now.Add(-4 * time.Minute)},
	}
	v := &activityView{}
	v.agents = agents
	v.hosts = 3
	v.plan = planUsage{
		FiveHour: &planWindow{Used: 62, ResetAt: now.Add(2*time.Hour + 10*time.Minute)},
		Weekly:   &planWindow{Used: 41, ResetAt: now.Add(4 * 24 * time.Hour)},
	}
	v.hasPlan = true
	return v
}
