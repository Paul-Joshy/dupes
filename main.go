// dupes — polished TUI for dedupe, built on Charm's bubbletea + lipgloss.
//
// Same engine semantics as dedupe.py: size-group → md5 (mtime-keyed shared
// cache) → one keeper per group; every removal is checksum-verified against
// its kept twin before the move; removals go to ~/.Trash with a TSV manifest
// in ~/.local/share/dedupe — never rm. Excludes tool-managed trees.
//
// Keys: j/k move · enter/e expand · space select · a all · n none ·
//       K make hovered member keeper · d trash (y/N confirm) ·
//       o reveal in Finder · O open in yazi ·
//       / filter · g/G top/bottom · r rescan · q quit

package main

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const (
	cachePath  = "~/.cache/dedupe-hashes.tsv"
	manifestD  = "~/.local/share/dedupe"
	trashPath  = "~/.Trash"
	defaultDoc = "~/Documents"
)

var excludeDirs = map[string]bool{
	"immich-app": true, "node_modules": true, ".git": true,
	".venv": true, "venv": true, "__pycache__": true, ".npm": true,
	".cache": true, ".build": true, "target": true, ".Trash": true,
}

// ---------- engine ----------

type cacheEnt struct {
	size  int64
	mtime int64
	md5   string
}

func loadCache() map[string]cacheEnt {
	m := map[string]cacheEnt{}
	b, err := os.ReadFile(expand(cachePath))
	if err != nil {
		return m
	}
	for _, line := range strings.Split(string(b), "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) != 4 {
			continue
		}
		var s, mt int64
		fmt.Sscanf(parts[1], "%d", &s)
		fmt.Sscanf(parts[2], "%d", &mt)
		m[parts[0]] = cacheEnt{s, mt, parts[3]}
	}
	return m
}

func saveCache(m map[string]cacheEnt) {
	f, err := os.Create(expand(cachePath))
	if err != nil {
		return
	}
	defer f.Close()
	for p, e := range m {
		fmt.Fprintf(f, "%s\t%d\t%d\t%s\n", p, e.size, e.mtime, e.md5)
	}
}

func md5Of(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func expand(p string) string {
	if strings.HasPrefix(p, "~") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[1:])
	}
	return p
}

type Group struct {
	paths    []string // sorted; paths[keeperIdx] is the keeper
	keeperIx int
	sel      bool
}

func (g *Group) size() int64 {
	st, err := os.Stat(g.paths[0])
	if err != nil {
		return 0
	}
	return st.Size()
}

func (g *Group) wasted() int64 { return g.size() * int64(len(g.paths)-1) }

func (g *Group) keeper() string { return g.paths[g.keeperIx] }

func (g *Group) extras() []string {
	out := []string{}
	for i, p := range g.paths {
		if i != g.keeperIx {
			out = append(out, p)
		}
	}
	return out
}

// ---------- messages ----------

type progMsg struct {
	phase string // "scanning" | "hashing" | "trashing"
	n     int
	total int
}

type scanDoneMsg struct {
	groups []*Group
	cache  map[string]cacheEnt
	err    string
}

type trashDoneMsg struct {
	moved    int
	freed    int64
	errs     []string
	manifest string
}

// sent when a subprocess (yazi) hands the terminal back
type resumeMsg struct{ err error }

// ---------- config ----------

type Config struct {
	Targets       []string          `toml:"targets"`
	Exclude       []string          `toml:"exclude"`
	BarWidth      int               `toml:"bar_width"`
	ShowFullPaths bool              `toml:"show_full_paths"`
	Keybinds      map[string][]string `toml:"keybinds"`
}

// every action and its default keys (bubbletea key names)
var defaultKeybinds = map[string][]string{
	"quit":         {"q", "ctrl+c"},
	"down":         {"j", "down"},
	"up":           {"k", "up"},
	"top":          {"g", "home"},
	"bottom":       {"G", "end"},
	"expand":       {"enter", "e"},
	"select":       {" "},
	"select_all":   {"a"},
	"select_none":  {"n"},
	"keeper":       {"K"},
	"trash":        {"d"},
	"finder":       {"o"},
	"yazi":         {"O"},
	"filter":       {"/"},
	"clear_filter": {"esc"},
	"rescan":       {"r"},
}

// merge user keybinds over the defaults, ignoring unknown actions
func mergedKeybinds(user map[string][]string) map[string][]string {
	kb := map[string][]string{}
	for a, keys := range defaultKeybinds {
		kb[a] = keys
	}
	for a, keys := range user {
		if _, known := defaultKeybinds[a]; known && len(keys) > 0 {
			kb[a] = keys
		}
	}
	return kb
}

// invert action→keys into key→action for dispatch
func keyToAction(kb map[string][]string) map[string]string {
	m := map[string]string{}
	for a, keys := range kb {
		for _, k := range keys {
			m[k] = a
		}
	}
	return m
}

// displayKey makes internal key names readable in the footer.
func displayKey(k string) string {
	if k == " " {
		return "space"
	}
	return k
}

// primary key for an action, for the footer hints
func primaryKey(kb map[string][]string, action string) string {
	if keys := kb[action]; len(keys) > 0 {
		return displayKey(keys[0])
	}
	return "?"  // intentionally odd so a missing action is visible
}

const configPath = "~/.config/dupes/config.toml"

const defaultConfig = `# dupes configuration (~/.config/dupes/config.toml)

# Directories to scan. With more than one, a picker is shown at startup.
# Paths given on the command line override this.
targets = ["~/Documents"]

# Extra directory names to skip anywhere in the tree, on top of the
# built-ins: .git, node_modules, venv, .venv, immich-app, __pycache__,
# .npm, .cache, .build, target
exclude = []

# Width of the waste bar, in characters.
bar_width = 18

# true = show complete paths; false = compact top-dir/initials/filename style
show_full_paths = false

# Rebind any action by listing one or more keys (bubbletea key names:
# "space", "enter", "esc", "ctrl+x", or plain letters). Only the actions
# you list are overridden; everything else keeps its default.
[keybinds]
# down = ["j", "down"]
# up = ["k", "up"]
# quit = ["q", "ctrl+c"]
`

func loadConfig() Config {
	c := Config{BarWidth: 18}
	path := configPath
	if p := os.Getenv("DUPES_CONFIG"); p != "" {
		path = p
	}
	b, err := os.ReadFile(expand(path))
	if os.IsNotExist(err) {
		os.MkdirAll(filepath.Dir(expand(path)), 0o755)
		os.WriteFile(expand(path), []byte(defaultConfig), 0o644)
	} else if err == nil {
		toml.Unmarshal(b, &c)
	}
	if c.BarWidth <= 0 || c.BarWidth > 60 {
		c.BarWidth = 18
	}
	if len(c.Targets) == 0 {
		c.Targets = []string{"~/Documents"}
	}
	return c
}

// ---------- path display ----------

func tilde(p string) string {
	home := expand("~")
	if strings.HasPrefix(p, home) {
		return "~" + p[len(home):]
	}
	return p
}

// abbreviate collapses every directory component to its first character,
// keeps the filename intact, and middle-truncates a very long filename:
//   ~/Documents/ai-job-search/documents/report_final_v2.pdf
//   → ~/D/a/report_final_v2.pdf
func abbreviate(p string) string {
	parts := strings.Split(p, "/")
	if len(parts) <= 3 {
		return p
	}
	out := []string{parts[0]}
	for _, seg := range parts[1 : len(parts)-1] {
		if seg == "" {
			continue
		}
		out = append(out, string([]rune(seg)[0]))
	}
	last := parts[len(parts)-1]
	if runes := []rune(last); len(runes) > 48 {
		last = string(runes[:24]) + "…" + string(runes[len(runes)-16:])
	}
	return strings.Join(append(out, last), "/")
}

// abbreviateRel keeps the top-level directory readable (that's the folder
// you actually care about), collapses deeper directories to their first
// character, keeps the filename intact, and middle-truncates a very long
// filename:  ai-job-search/documents/portfolio/file.pdf → ai-job-search/d/p/file.pdf
func abbreviateRel(rel string) string {
	parts := strings.Split(rel, "/")
	if len(parts) == 1 {
		return rel
	}
	last := parts[len(parts)-1]
	if runes := []rune(last); len(runes) > 48 {
		last = string(runes[:24]) + "…" + string(runes[len(runes)-16:])
	}
	out := []string{}
	for i, seg := range parts[:len(parts)-1] {
		if seg == "" {
			continue
		}
		if i == 0 {
			// keep the top-level directory recognizable
			if runes := []rune(seg); len(runes) > 24 {
				seg = string(runes[:18]) + "…" + string(runes[len(runes)-6:])
			}
			out = append(out, seg)
		} else {
			out = append(out, string([]rune(seg)[0]))
		}
	}
	return strings.Join(append(out, last), "/")
}

// ---------- model ----------

type model struct {
	root     string
	cfg      Config
	targets  []string // >1 target → picker phase first
	phase    string   // pick | loading | scanning | hashing | ready | trashing | done
	pickCur  int
	keymap   map[string]string // key press → action name
	groups   []*Group
	cache    map[string]cacheEnt
	progN    int
	progTot  int
	cursor   int
	expanded map[*Group]bool
	filter   string
	filterIn string
	typing   bool
	confirm  bool
	width    int
	height   int
	status   string // transient footer message
	errs     []string
	quitting bool
}

func initialModel(cfg Config, targets []string) model {
	kb := mergedKeybinds(cfg.Keybinds)
	phase := "loading"
	if len(targets) > 1 {
		phase = "pick"
	}
	return model{
		cfg:      cfg,
		targets:  targets,
		phase:    phase,
		keymap:   keyToAction(kb),
		cache:    map[string]cacheEnt{},
		expanded: map[*Group]bool{},
	}
}

func (m model) Init() tea.Cmd {
	if m.phase == "pick" {
		return nil
	}
	return scanCmd(m.root)
}

// ---------- scan / trash commands ----------

// findGroups runs the whole scan→hash→group pipeline for one root and
// returns the duplicate groups plus the (possibly updated) hash cache.
// notify (may be nil) receives phase/progress updates.
func findGroups(root string, notify func(phase string, n, total int)) ([]*Group, map[string]cacheEnt, error) {
	cache := loadCache()
	changed := false
	send := func(phase string, n, total int) {
		if notify != nil {
			notify(phase, n, total)
		}
	}

	// walk
	sizeMap := map[int64][]string{}
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			name := info.Name()
			if path != root && excludeDirs[name] {
				return filepath.SkipDir
			}
			return nil
		}
		if info.Name() == ".DS_Store" || info.Size() == 0 {
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		sizeMap[info.Size()] = append(sizeMap[info.Size()], path)
		return nil
	})
	send("hashing", 0, 0)

	total := 0
	for _, paths := range sizeMap {
		if len(paths) >= 2 {
			total += len(paths)
		}
	}
	hashes := map[string]string{}
	lastSend := time.Now()
	n := 0
	for size, paths := range sizeMap {
		if len(paths) < 2 {
			continue
		}
		for _, path := range paths {
			n++
			if time.Since(lastSend) > 80*time.Millisecond {
				lastSend = time.Now()
				send("hashing", n, total)
			}
			st, err := os.Stat(path)
			if err != nil {
				continue
			}
			mt := st.ModTime().UnixNano()
			if c, ok := cache[path]; ok && c.size == size && c.mtime == mt {
				hashes[path] = c.md5
				continue
			}
			h, err := md5Of(path)
			if err != nil {
				continue
			}
			cache[path] = cacheEnt{size, mt, h}
			hashes[path] = h
			changed = true
		}
	}
	if changed {
		saveCache(cache) // full merged write — never clobbers other entries
	}
	send("hashing", total, total)

	// group by md5
	byHash := map[string][]string{}
	for path, h := range hashes {
		byHash[h] = append(byHash[h], path)
	}
	var groups []*Group
	for _, paths := range byHash {
		if len(paths) < 2 {
			continue
		}
		sort.Strings(paths)
		groups = append(groups, &Group{paths: paths})
	}
	sort.Slice(groups, func(i, j int) bool {
		wi, wj := groups[i].wasted(), groups[j].wasted()
		if wi != wj {
			return wi > wj
		}
		return groups[i].paths[0] < groups[j].paths[0]
	})
	return groups, cache, nil
}

func scanCmd(root string) tea.Cmd {
	p := activeProg
	return func() tea.Msg {
		notify := func(phase string, n, total int) {
			if p != nil {
				p.Send(progMsg{phase, n, total})
			}
		}
		groups, cache, err := findGroups(root, notify)
		if err != nil {
			return scanDoneMsg{err: err.Error()}
		}
		return scanDoneMsg{groups: groups, cache: cache}
	}
}

// trashGroups moves the non-keeper files of each group to ~/.Trash with a
// TSV manifest, re-verifying every md5 against its kept twin first.
// notify (may be nil) receives done/total progress. Returns the count of
// files moved, bytes freed, per-file errors, and the manifest path.
func trashGroups(groups []*Group, notify func(done, total int)) (moved int, freed int64, errs []string, manifest string, err error) {
	os.MkdirAll(expand(manifestD), 0o755)
	manifest = filepath.Join(expand(manifestD),
		"removed-"+time.Now().Format("20060102-150405")+".tsv")
	mf, err := os.Create(manifest)
	if err != nil {
		return 0, 0, nil, "", fmt.Errorf("cannot write manifest: %w", err)
	}
	defer mf.Close()

	trash := expand(trashPath)
	total := 0
	for _, g := range groups {
		total += len(g.extras())
	}
	done := 0
	for _, g := range groups {
		for _, extra := range g.extras() {
			done++
			if notify != nil {
				notify(done, total)
			}
			// safety invariant: re-verify against the kept twin before moving
			hx, err := md5Of(extra)
			if err != nil {
				errs = append(errs, "skip (unreadable): "+extra)
				continue
			}
			hk, err := md5Of(g.keeper())
			if err != nil {
				errs = append(errs, "skip (twin unreadable): "+extra)
				continue
			}
			if hx != hk {
				errs = append(errs, "skip (twin changed): "+extra)
				continue
			}
			base := filepath.Base(extra)
			stem, ext := "", filepath.Ext(base)
			stem = strings.TrimSuffix(base, ext)
			dst := filepath.Join(trash, base)
			for i := 1; ; i++ {
				if _, err := os.Lstat(dst); os.IsNotExist(err) {
					break
				}
				dst = filepath.Join(trash, fmt.Sprintf("%s (%d)%s", stem, i, ext))
			}
			if err := os.Rename(extra, dst); err != nil {
				errs = append(errs, "failed: "+extra+" — "+err.Error())
				continue
			}
			fmt.Fprintf(mf, "%s\t%s\t%d\n", extra, g.keeper(), g.size())
			moved++
			freed += g.size()
		}
		// drop trashed paths from the group
		var left []string
		for _, path := range g.paths {
			if _, err := os.Lstat(path); err == nil {
				left = append(left, path)
			}
		}
		g.paths = left
		g.keeperIx = 0
		g.sel = false
	}
	return moved, freed, errs, manifest, nil
}

func trashCmd(groups []*Group) tea.Cmd {
	p := activeProg
	return func() tea.Msg {
		moved, freed, errs, manifest, _ := trashGroups(groups, func(done, total int) {
			if p != nil {
				p.Send(progMsg{"trashing", done, total})
			}
		})
		return trashDoneMsg{moved: moved, freed: freed, errs: errs, manifest: manifest}
	}
}

// hoveredPath is the file under the cursor: the member on a member row,
// the keeper on a group header row.
func (m model) hoveredPath() string {
	rows := m.visibleRows()
	if m.cursor < 0 || m.cursor >= len(rows) {
		return ""
	}
	r := rows[m.cursor]
	if r.member >= 0 {
		return r.g.paths[r.member]
	}
	return r.g.keeper()
}

// ---------- update ----------

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height

	case progMsg:
		m.phase = msg.phase
		m.progN, m.progTot = msg.n, msg.total

	case scanDoneMsg:
		if msg.err != "" {
			m.errs = []string{msg.err}
			m.phase = "ready"
			return m, nil
		}
		m.groups = msg.groups
		m.cache = msg.cache
		m.phase = "ready"
		m.cursor = 0
		m.expanded = map[*Group]bool{}
		if len(m.groups) == 0 {
			m.status = "no duplicates found 🎉"
		}

	case trashDoneMsg:
		m.phase = "ready"
		m.errs = msg.errs
		m.confirm = false
		// drop groups that fell below 2 members
		var keep []*Group
		for _, g := range m.groups {
			if len(g.paths) >= 2 {
				keep = append(keep, g)
			}
		}
		m.groups = keep
		m.clampCursor()
		note := fmt.Sprintf("trashed %d files · %s reclaimed · manifest %s",
			msg.moved, human(msg.freed), filepath.Base(msg.manifest))
		if len(msg.errs) > 0 {
			note += fmt.Sprintf(" · ⚠ %d skipped", len(msg.errs))
		}
		m.status = note

	case resumeMsg:
		if msg.err != nil {
			m.status = "yazi exited with an error: " + msg.err.Error()
		} else {
			m.status = "back from yazi — press r to rescan if files were removed"
		}
		// force a full repaint in case the hand-back left a stale frame
		return m, tea.ClearScreen

	case tea.KeyMsg:
		k := msg.String()
		if m.phase == "pick" {
			switch k {
			case "q", "esc", "ctrl+c":
				m.quitting = true
				return m, tea.Quit
			case "j", "down":
				m.pickCur++
				if m.pickCur >= len(m.targets) {
					m.pickCur = len(m.targets) - 1
				}
			case "k", "up":
				m.pickCur--
				if m.pickCur < 0 {
					m.pickCur = 0
				}
			case "g":
				m.pickCur = 0
			case "G":
				m.pickCur = len(m.targets) - 1
			case "enter":
				m.root = m.targets[m.pickCur]
				m.phase = "loading"
				return m, scanCmd(m.root)
			}
			return m, nil
		}
		if m.typing {
			switch k {
			case "esc":
				m.typing = false
				m.filterIn = ""
			case "enter":
				m.typing = false
				m.filter = m.filterIn
				m.cursor = 0
			case "backspace":
				if len(m.filterIn) > 0 {
					r := []rune(m.filterIn)
					m.filterIn = string(r[:len(r)-1])
				}
			default:
				if len(k) == 1 && k >= " " {
					m.filterIn += k
				}
			}
			return m, nil
		}
		if m.confirm {
			switch k {
			case "y", "Y":
				var targets []*Group
				for _, g := range m.visibleGroups() {
					if g.sel {
						targets = append(targets, g)
					}
				}
				if len(targets) == 0 {
					if g := m.hoveredGroup(); g != nil {
						targets = []*Group{g}
					}
				}
				if len(targets) > 0 {
					m.phase = "trashing"
					return m, trashCmd(targets)
				}
				m.confirm = false
			default:
				m.confirm = false
			}
			return m, nil
		}

		action, known := m.keymap[k]
		if !known {
			return m, nil
		}
		switch action {
		case "quit":
			m.quitting = true
			return m, tea.Quit
		case "down":
			m.cursor++
			m.clampCursor()
			m.status = m.hoverStatus()
		case "up":
			m.cursor--
			m.clampCursor()
			m.status = m.hoverStatus()
		case "top":
			m.cursor = 0
			m.status = m.hoverStatus()
		case "bottom":
			m.cursor = 1 << 30
			m.clampCursor()
			m.status = m.hoverStatus()
		case "expand":
			if g := m.hoveredGroup(); g != nil {
				m.expanded[g] = !m.expanded[g]
			}
		case "select":
			if g := m.hoveredGroup(); g != nil {
				g.sel = !g.sel
			}
		case "select_all":
			for _, g := range m.visibleGroups() {
				g.sel = true
			}
		case "select_none":
			for _, g := range m.groups {
				g.sel = false
			}
		case "keeper":
			// The keeper is an index into the sorted paths — just move it.
			if gi, mi := m.hoveredMember(); gi != nil && mi >= 0 {
				gi.keeperIx = mi
			}
		case "finder":
			if p := m.hoveredPath(); p != "" {
				if err := exec.Command("open", "-R", p).Start(); err != nil {
					m.status = "cannot open Finder: " + err.Error()
				} else {
					m.status = "revealed in Finder: " + filepath.Base(p)
				}
			}
		case "yazi":
			if p := m.hoveredPath(); p != "" {
				if _, err := exec.LookPath("yazi"); err != nil {
					m.status = "yazi is not installed"
					return m, nil
				}
				// hand the terminal to yazi hovering this exact file; dupes
				// resumes when yazi quits (yazi may trash files → rescan hint)
				cmd := exec.Command("yazi", p)
				return m, tea.ExecProcess(cmd, func(err error) tea.Msg {
					return resumeMsg{err}
				})
			}
		case "trash":
			if len(m.visibleGroups()) > 0 {
				m.confirm = true
			}
		case "filter":
			m.typing = true
			m.filterIn = ""
		case "clear_filter":
			m.filter = ""
			m.cursor = 0
		case "rescan":
			m.phase = "loading"
			m.groups = nil
			return m, scanCmd(m.root)
		}
	}
	return m, nil
}

// keymapKB returns the merged action→keys map (defaults + user config),
// used for the dynamic footer hints.
func (m model) keymapKB() map[string][]string {
	return mergedKeybinds(m.cfg.Keybinds)
}

// hoverStatus shows the full location of the hovered file in the status
// line, so compact rows never leave you guessing where a file really is.
func (m model) hoverStatus() string {
	p := m.hoveredPath()
	if p == "" {
		return ""
	}
	label := "keeper"
	if _, mi := m.hoveredMember(); mi >= 0 {
		label = "member"
	}
	return tilde(p) + "  (" + label + ")"
}

func (m *model) clampCursor() {
	rows := m.visibleRows()
	if m.cursor >= len(rows) {
		m.cursor = len(rows) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

func (m model) visibleGroups() []*Group {
	f := strings.ToLower(m.filter)
	var out []*Group
	for _, g := range m.groups {
		if f == "" {
			out = append(out, g)
			continue
		}
		for _, p := range g.paths {
			if strings.Contains(strings.ToLower(p), f) {
				out = append(out, g)
				break
			}
		}
	}
	return out
}

func (m model) hoveredGroup() *Group {
	rows := m.visibleRows()
	if m.cursor < 0 || m.cursor >= len(rows) {
		return nil
	}
	return rows[m.cursor].g
}

// hoveredMember returns (group, member index) if the cursor sits on a member row.
func (m model) hoveredMember() (*Group, int) {
	rows := m.visibleRows()
	if m.cursor < 0 || m.cursor >= len(rows) {
		return nil, -1
	}
	return rows[m.cursor].g, rows[m.cursor].member
}

type row struct {
	g      *Group
	member int // -1 = group header row
}

func (m model) visibleRows() []row {
	var rows []row
	for _, g := range m.visibleGroups() {
		rows = append(rows, row{g, -1})
		if m.expanded[g] {
			for i := range g.paths {
				rows = append(rows, row{g, i})
			}
		}
	}
	return rows
}

// ---------- view ----------

var (
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("#4A4468")).Padding(0, 1)
	subStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	dimStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	errStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	okStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("114"))
	selStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("51")).Bold(true)
	barColors  = []lipgloss.Color{"#F87171", "#FB923C", "#FACC15", "#4ADE80", "#60A5FA"}
	footerKey  = lipgloss.NewStyle().Foreground(lipgloss.Color("173")).Bold(true)
	footerTxt  = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
)

func barColor(i int) lipgloss.Color {
	if i >= len(barColors) {
		i = len(barColors) - 1
	}
	return barColors[i]
}

func human(b int64) string {
	switch {
	case b >= 1e12:
		return fmt.Sprintf("%.2f TB", float64(b)/1e12)
	case b >= 1e9:
		return fmt.Sprintf("%.2f GB", float64(b)/1e9)
	case b >= 1e6:
		return fmt.Sprintf("%.1f MB", float64(b)/1e6)
	case b >= 1e3:
		return fmt.Sprintf("%.1f KB", float64(b)/1e3)
	default:
		return fmt.Sprintf("%d B", b)
	}
}

func (m model) header() string {
	switch m.phase {
	case "loading":
		return titleStyle.Render("dupes "+tilde(m.root)) + "  " + subStyle.Render("scanning…")
	case "scanning":
		return titleStyle.Render("dupes "+tilde(m.root)) + "  " + subStyle.Render("scanning… (cache makes reruns fast)")
	case "hashing":
		return titleStyle.Render("dupes "+tilde(m.root)) + "  " +
			subStyle.Render(fmt.Sprintf("hashing… %s/%s files", comma(m.progN), comma(m.progTot)))
	case "trashing":
		return titleStyle.Render("dupes "+tilde(m.root)) + "  " +
			errStyle.Render(fmt.Sprintf("trashing… %d/%d (checksum-verifying each)", m.progN, m.progTot))
	}
	var ng, nw, ns, sw int
	var wsel int64
	for _, g := range m.visibleGroups() {
		ng++
		nw += len(g.paths) - 1
		sw += int(g.wasted())
		if g.sel {
			ns++
			wsel += g.wasted()
		}
	}
	t := fmt.Sprintf("dupes %s — %s groups · %s wasted", tilde(m.root), comma(ng), human(int64(sw)))
	title := titleStyle.Render(t)
	sel := subStyle.Render(fmt.Sprintf(" · selected %s (%s)", comma(ns), human(wsel)))
	f := ""
	if m.filter != "" {
		f = "  " + okStyle.Render("/"+m.filter)
	}
	return title + sel + f
}

func (m model) footer() string {
	if m.typing {
		return footerKey.Render("filter") + footerTxt.Render(": ") + okStyle.Render(m.filterIn+"▌") +
			footerTxt.Render("  enter apply · esc cancel")
	}
	hints := []struct{ keys, v string }{
		{primaryKey(m.keymapKB(), "down") + "/" + primaryKey(m.keymapKB(), "up"), "move"},
		{primaryKey(m.keymapKB(), "expand"), "expand"},
		{primaryKey(m.keymapKB(), "select"), "select"},
		{primaryKey(m.keymapKB(), "select_all") + "/" + primaryKey(m.keymapKB(), "select_none"), "all/none"},
		{primaryKey(m.keymapKB(), "keeper"), "keeper"},
		{primaryKey(m.keymapKB(), "trash"), "trash"},
		{primaryKey(m.keymapKB(), "finder"), "finder"},
		{primaryKey(m.keymapKB(), "yazi"), "yazi"},
		{primaryKey(m.keymapKB(), "filter"), "filter"},
		{primaryKey(m.keymapKB(), "rescan"), "rescan"},
		{primaryKey(m.keymapKB(), "quit"), "quit"},
	}
	parts := []string{}
	for _, h := range hints {
		parts = append(parts, footerKey.Render(h.keys)+" "+footerTxt.Render(h.v))
	}
	line := strings.Join(parts, footerTxt.Render(" · "))
	if m.status != "" {
		return line + "\n" + dimStyle.Render(m.status)
	}
	return line
}

func (m model) View() string {
	if m.width == 0 {
		return "starting…"
	}
	if m.phase == "pick" {
		return m.pickView()
	}
	var b strings.Builder
	b.WriteString(m.header() + "\n")

	if m.confirm {
		return m.confirmView()
	}
	if m.phase != "ready" && m.phase != "done" {
		if m.status != "" {
			b.WriteString(dimStyle.Render(m.status) + "\n")
		}
		b.WriteString("\n")
		return b.String() + m.footer()
	}

	rows := m.visibleRows()
	if len(rows) == 0 && len(m.groups) > 0 {
		b.WriteString(dimStyle.Render("  no groups match filter") + "\n")
	}
	// scroll window
	maxRows := m.height - 4 // header + footer(+status)
	if maxRows < 1 {
		maxRows = 1
	}
	start := 0
	if len(rows) > maxRows {
		start = m.cursor - maxRows/2
		if start < 0 {
			start = 0
		}
		if start > len(rows)-maxRows {
			start = len(rows) - maxRows
		}
	}
	end := start + maxRows
	if end > len(rows) {
		end = len(rows)
	}

	// max waste for bar scaling
	var maxW int64
	for _, g := range m.visibleGroups() {
		if g.wasted() > maxW {
			maxW = g.wasted()
		}
	}
	for i := start; i < end; i++ {
		r := rows[i]
		curs := ""
		if i == m.cursor {
			curs = "▸ "
		} else {
			curs = "  "
		}
		if r.member == -1 {
			b.WriteString(m.groupRow(r.g, curs, maxW) + "\n")
		} else {
			b.WriteString(m.memberRow(r.g, r.member, curs) + "\n")
		}
	}
	if m.status != "" && len(m.errs) > 0 {
		for _, e := range m.errs {
			b.WriteString(errStyle.Render("  "+e) + "\n")
		}
	}
	return b.String() + m.footer()
}

func (m model) groupRow(g *Group, curs string, maxW int64) string {
	ind := int64(m.cfg.BarWidth)
	filled := int64(0)
	if maxW > 0 {
		filled = g.wasted() * ind / maxW
	}
	if filled < 1 {
		filled = 1
	}
	bar := barStyle(barColor(rankOf(m, g))).Render(strings.Repeat("█", int(filled)) +
		strings.Repeat("╌", int(ind-filled)))
	selMark := "○"
	s := lipgloss.NewStyle()
	if g.sel {
		selMark = "●"
		s = selStyle
	}
	mark := "▸"
	if m.expanded[g] {
		mark = "▾"
	}
	return fmt.Sprintf("%s%s %s %s %s ×%d  %s",
		curs, s.Render(selMark), bar, human(g.size()), dimStyle.Render(mark), len(g.paths), m.pathOf(g.keeper()))
}

func rankOf(m model, g *Group) int {
	r := 0
	var prev int64 = -1
	for _, gg := range m.visibleGroups() {
		if gg.wasted() != prev {
			r++
			prev = gg.wasted()
		}
		if gg == g {
			return r - 1
		}
	}
	return 99
}

func barStyle(c lipgloss.Color) lipgloss.Style {
	return lipgloss.NewStyle().Foreground(c)
}

func (m model) memberRow(g *Group, i int, curs string) string {
	mark := dimStyle.Render("·")
	style := dimStyle
	if i == g.keeperIx {
		mark = okStyle.Render("✓")
		style = lipgloss.NewStyle()
	}
	return fmt.Sprintf("%s   %s %s", curs, mark, style.Render(m.pathOf(g.paths[i])))
}

// pathOf applies the configured path display style. Relative to the scan
// root (already in the title), with directories collapsed to first letters.
func (m model) pathOf(p string) string {
	if m.cfg.ShowFullPaths {
		return tilde(p)
	}
	if m.root != "" && strings.HasPrefix(p, m.root+"/") {
		return abbreviateRel(p[len(m.root)+1:])
	}
	return abbreviate(tilde(p))
}

func (m model) pickView() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("dupes — choose a directory to scan") + "\n\n")
	for i, t := range m.targets {
		curs := "  "
		style := dimStyle
		if i == m.pickCur {
			curs = "▸ "
			style = lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Bold(true)
		}
		b.WriteString(fmt.Sprintf("%s%s\n", curs, style.Render(tilde(t))))
	}
	b.WriteString("\n" + footerKey.Render("j/k") + footerTxt.Render(" move · ") +
		footerKey.Render("enter") + footerTxt.Render(" scan · ") +
		footerKey.Render("q") + footerTxt.Render(" quit"))
	return b.String()
}

func (m model) confirmView() string {
	n, files, bytes := 0, 0, int64(0)
	for _, g := range m.visibleGroups() {
		if g.sel {
			n++
			files += len(g.extras())
			bytes += g.wasted()
		}
	}
	if n == 0 {
		if g := m.hoveredGroup(); g != nil {
			n = 1
			files = len(g.extras())
			bytes = g.wasted()
		}
	}
	if n == 0 {
		return "nothing to trash\n" + m.footer()
	}
	body := fmt.Sprintf("  Trash %d group(s) → %d files → %s reclaimed?\n", n, files, human(bytes)) +
		"  every file re-verified against its twin before moving\n" +
		"  removals go to ~/.Trash with a restore manifest\n\n" +
		footerKey.Render("  y") + footerTxt.Render(" confirm · ") + footerKey.Render("esc/N") + footerTxt.Render(" cancel")
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("203")).
		Padding(1, 2).
		Render(body) + "\n" + m.footer()
}


func comma(n int) string {
	s := fmt.Sprintf("%d", n)
	var out []string
	for len(s) > 3 {
		out = append([]string{s[len(s)-3:]}, out...)
		s = s[:len(s)-3]
	}
	out = append([]string{s}, out...)
	return strings.Join(out, ",")
}

// ---------- main ----------

var activeProg *tea.Program

const version = "0.1.0"

const usage = `dupes — find duplicate files, safely

Usage:
  dupes [paths…]              interactive TUI (paths override config targets)
  dupes --report [paths…]     print a waste report and exit (no TUI)
  dupes --trash [paths…]      trash every non-first twin of every group, then exit
  dupes --help | --version

Safety: nothing is ever deleted — removals move to ~/.Trash with a TSV
manifest recording where each file came from; every file is re-hashed and
compared to its kept twin immediately before moving. Tool-managed trees
(.git, node_modules, venv, immich-app, …) are never touched.

Config: ~/.config/dupes/config.toml  (targets, exclude, keybinds, …)
`

func main() {
	args := os.Args[1:]
	mode := "tui"
	var paths []string
	for _, a := range args {
		switch a {
		case "--report":
			mode = "report"
		case "--trash":
			mode = "trash"
		case "--help", "-h":
			fmt.Print(usage)
			return
		case "--version", "-v":
			fmt.Println("dupes " + version)
			return
		default:
			if strings.HasPrefix(a, "-") {
				fmt.Fprintf(os.Stderr, "dupes: unknown flag %s\n\n%s", a, usage)
				os.Exit(2)
			}
			paths = append(paths, a)
		}
	}

	cfg := loadConfig()
	for _, e := range cfg.Exclude {
		if e != "" {
			excludeDirs[e] = true
		}
	}

	// target resolution: CLI args > config
	targets := paths
	if len(targets) == 0 {
		targets = cfg.Targets
	}
	var valid []string
	for _, t := range targets {
		t = expand(t)
		if st, err := os.Stat(t); err == nil && st.IsDir() {
			valid = append(valid, t)
		} else {
			fmt.Fprintln(os.Stderr, "dupes: skipping non-directory target:", t)
		}
	}
	if len(valid) == 0 {
		fmt.Fprintln(os.Stderr, "dupes: no valid target directories")
		os.Exit(1)
	}

	switch mode {
	case "report":
		runReport(valid)
	case "trash":
		runTrash(valid)
	default:
		m := initialModel(cfg, valid)
		if len(valid) == 1 {
			m.root = valid[0]
		}
		activeProg = tea.NewProgram(m, tea.WithAltScreen())
		if _, err := activeProg.Run(); err != nil {
			fmt.Fprintln(os.Stderr, "dupes:", err)
			os.Exit(1)
		}
	}
}

// runReport scans each target and prints a plain-text summary of the
// duplicate groups, biggest waste first. Interactive: no.
func runReport(roots []string) {
	totalWasted := int64(0)
	totalGroups := 0
	for _, root := range roots {
		groups, _, err := findGroups(root, func(phase string, n, total int) {
			fmt.Fprintf(os.Stderr, "\r%s %d/%d", phase, n, total)
		})
		fmt.Fprint(os.Stderr, "\r\033[K")
		if err != nil {
			fmt.Fprintln(os.Stderr, "dupes:", err)
			os.Exit(1)
		}
		if len(roots) > 1 {
			fmt.Printf("\n# %s\n", root)
		}
		for _, g := range groups {
			totalGroups++
			totalWasted += g.wasted()
			fmt.Printf("%10s  ×%d  %s\n",
				human(g.wasted()), len(g.paths), strings.Join(g.paths, "  +  "))
		}
	}
	fmt.Printf("\n%d groups · %s wasted\n", totalGroups, human(totalWasted))
}

// runTrash is the non-interactive cleanup: for every group, keep the first
// path (sorted) and trash the rest — with the same md5 re-verification and
// manifest as the TUI. Requires a confirmation prompt on stdin.
func runTrash(roots []string) {
	var groups []*Group
	for _, root := range roots {
		gs, _, err := findGroups(root, func(phase string, n, total int) {
			fmt.Fprintf(os.Stderr, "\r%s %d/%d", phase, n, total)
		})
		fmt.Fprint(os.Stderr, "\r\033[K")
		if err != nil {
			fmt.Fprintln(os.Stderr, "dupes:", err)
			os.Exit(1)
		}
		groups = append(groups, gs...)
	}
	if len(groups) == 0 {
		fmt.Println("no duplicates found — nothing to do")
		return
	}
	wasted := int64(0)
	files := 0
	for _, g := range groups {
		wasted += g.wasted()
		files += len(g.extras())
	}
	fmt.Printf("%d groups · %d files would move to ~/.Trash · %s reclaimed\n",
		len(groups), files, human(wasted))
	fmt.Print("trash them? [y/N] ")
	var ans string
	fmt.Scanln(&ans)
	if ans != "y" && ans != "Y" {
		fmt.Println("cancelled — nothing moved")
		return
	}
	moved, freed, errs, manifest, err := trashGroups(groups, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dupes:", err)
		os.Exit(1)
	}
	for _, e := range errs {
		fmt.Fprintln(os.Stderr, "  "+e)
	}
	fmt.Printf("moved %d files to Trash · %s freed · manifest: %s\n",
		moved, human(freed), manifest)
}
