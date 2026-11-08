package main

// `rex-activity rex-install` puts Activity in Rex's command palette: a Lua
// file beside init.lua, and one line in init.lua that loads it.

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const rexLua = `-- Written by rex-activity rex-install. Running it again replaces this file.
-- init.lua loads it with: pcall(require, "rex_activity")
local exe = %q
local label = "Activity"
local flavor = "com.superlogical.terminal.shell"

local function need(ctx, title)
  if not ctx.session_id then error("run " .. title .. " from a Rex window", 0) end
end

local function block(argv)
  return { block = { flavor = flavor, label = label, options = { command = argv, shell = "none", login = false } } }
end

-- Activity in this session's tab: as a pane (floating false) or a floating
-- window (floating true).
local function find(ctx, floating)
  local view = rex.call("session.view", { session_id = ctx.session_id })
  for _, w in ipairs((view and view.windows) or {}) do
    if w.window_id == view.active_window_id then
      for _, l in ipairs(w.layers or {}) do
        if (l.kind == "floating") == floating then
          for _, b in ipairs(l.blocks or {}) do
            if b.label == label then return b, l end
          end
        end
      end
    end
  end
end

local function check(r, err)
  if err ~= nil then error(err, 0) end
  return r
end

rex.action {
  name = "rex_activity_pane",
  title = "Activity: Toggle Pane",
  category = "Activity",
  description = "Open Activity in a pane beside this one, or close it.",
  run = function(ctx)
    need(ctx, "Activity")
    local b = find(ctx, false)
    if b ~= nil then
      return check(rex.call("block.close", { session_id = ctx.session_id, block_id = b.block_id }))
    end
    if ctx.block_id == nil then error("focus a pane first", 0) end
    return check(rex.call("session.new_split", { session_id = ctx.session_id, anchor_id = ctx.block_id,
      direction = "vertical", side = "after", focus = false, layout = block({ exe }) }))
  end,
}

rex.action {
  name = "rex_activity_float",
  title = "Activity: Toggle Floating Window",
  category = "Activity",
  description = "Show or hide Activity floating over this tab.",
  run = function(ctx)
    need(ctx, "Activity")
    local _, l = find(ctx, true)
    if l ~= nil then
      local state = "minimized"
      if l.state == "minimized" then state = "normal" end
      return check(rex.call("session.set_layer_state", { session_id = ctx.session_id,
        layer_id = l.layer_id, state = state, focus = state == "normal" }))
    end
    return check(rex.call("session.new_layer", { session_id = ctx.session_id, focus = true,
      bounds = { 0.55, 0.04, 0.42, 0.92 }, layout = block({ exe }) }))
  end,
}

rex.action {
  name = "rex_activity_tab",
  title = "Activity: Open in a Tab",
  category = "Activity",
  description = "Open Activity in a tab of its own.",
  run = function(ctx)
    need(ctx, "Activity")
    return check(rex.call("session.new_window", { session_id = ctx.session_id, focus = true,
      layout = block({ exe }) }))
  end,
}
`

const requireLine = `pcall(require, "rex_activity")`

func rexConfigDir() string {
	if c := os.Getenv("REX_CONFIG"); c != "" {
		return filepath.Dir(c)
	}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "rex")
}

// runRexInstall is `rex-activity rex-install [--remove]`.
func runRexInstall(args []string) error {
	remove := len(args) > 0 && args[0] == "--remove"
	dir := rexConfigDir()
	luaPath := filepath.Join(dir, "rex_activity.lua")
	initPath := filepath.Join(dir, "init.lua")
	init, err := os.ReadFile(initPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	has := bytes.Contains(init, []byte(requireLine))

	if remove {
		os.Remove(luaPath)
		if has {
			var kept []string
			for _, l := range strings.Split(string(init), "\n") {
				if strings.TrimSpace(l) != requireLine {
					kept = append(kept, l)
				}
			}
			if err := writeAtomic(initPath, []byte(strings.Join(kept, "\n"))); err != nil {
				return err
			}
		}
		reloadRex()
		fmt.Println("Took Activity out of Rex's command palette.")
		return nil
	}

	self, err := os.Executable()
	if err != nil {
		return err
	}
	if p, err := filepath.EvalSymlinks(self); err == nil {
		self = p
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(luaPath, []byte(fmt.Sprintf(rexLua, self)), 0o644); err != nil {
		return err
	}
	if !has {
		if len(init) > 0 {
			if err := os.WriteFile(initPath+".before-rex-activity", init, 0o644); err != nil {
				return err
			}
		}
		out := append([]byte(nil), init...)
		if len(out) > 0 && !bytes.HasSuffix(out, []byte("\n")) {
			out = append(out, '\n')
		}
		out = append(out, []byte(requireLine+"\n")...)
		if err := os.WriteFile(initPath, out, 0o644); err != nil {
			return err
		}
	}
	reloadRex()
	fmt.Printf("Added Activity to Rex's command palette (%s).\n", luaPath)
	fmt.Println("Look for Activity: Toggle Pane, Toggle Floating Window, and Open in a Tab.")
	return nil
}

func reloadRex() {
	cmd := exec.Command(rexBin(), "config", "reload")
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	cmd.Run()
}
