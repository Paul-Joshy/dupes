# dupes

Find duplicate files on macOS and clean them up — **safely**.

`dupes` scans a directory for files with identical content, groups them,
and shows the groups sorted by how much disk space each one is wasting.
Deleting is never automatic: every removal is a **move to the Trash**, with a
manifest you can audit, and every file is **re-verified against its kept
twin immediately before moving**.
## Safety guarantees

These are the point of the tool, not an afterthought:

- **Nothing is ever deleted.** Removals are `rename(2)` into `~/.Trash` —
  recoverable, reversible, and never `rm`.
- **Checksum re-verification.** Before a file moves, its MD5 is recomputed
  and compared against the kept twin's *freshly recomputed* MD5. If either
  fails to hash or the hashes differ, the file is skipped, not moved.
- **Audit trail.** Every session writes a TSV manifest
  (`~/.local/share/dedupe/removed-<timestamp>.tsv`) with one line per move:
  `trashed_path → kept_twin  size`.
- **Tool-managed trees are never touched.** `.git`, `node_modules`, venvs,
  `__pycache__`, `immich-app`, `.npm`, `.cache`, `.build`, `target`, and the
  Trash itself are always skipped. Add your own with `exclude`.
- **macOS Trash semantics.** `os.Rename` into `~/.Trash` is an atomic move
  on the same volume — no copy-then-delete window.

## Install

```sh
go install github.com/Paul-Joshy/dupes@latest
```

Requires Go 1.24+ (developed on 1.26) and macOS — the Trash integration and
Finder reveal are macOS-specific. Linux works for scanning/reporting but
"Trash" mode moves to `~/.local/share/Trash/files` if present.

## Usage

```sh
dupes                     # scan config targets (default: ~/Documents)
dupes ~/Pictures          # scan a specific directory
dupes ~/A ~/B             # scan several dirs as ONE space — groups include
                          # duplicates within each dir AND across dirs
dupes --report ~/A        # print a plain-text waste report, no TUI
dupes --trash ~/A         # non-interactive cleanup (with a y/N prompt)
dupes --help | --version
```

## Keys

| Key | Action |
| --- | --- |
| `j` / `k` | move cursor (vim-style) |
| `enter` / `e` | expand/collapse a group |
| `space` | select a group for trashing |
| `a` / `n` | select all / none |
| `K` | make the hovered member the keeper |
| `d` | trash all selected groups (asks `y/N`) |
| `o` | reveal the hovered file in Finder |
| `O` | open the hovered file in [yazi](https://yazi-rs.github.io); dupes resumes when you quit |
| `/` | filter groups by path substring |
| `r` | rescan |
| `q` | quit |

Every one of these is rebindable — see below.

## Configuration

`~/.config/dupes/config.toml` is created with defaults on first run.

```toml
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
# quit = ["q", "esc"]
```

Set `DUPES_CONFIG=/path/to/config.toml` to use a different config file.

## How it works

1. Walk every target into one bucket-by-size map (cheap pre-filter); all
   targets are scanned together, so groups can span directories
2. Bucket files by size (cheap pre-filter)
3. MD5 only the buckets with ≥2 files, through a shared mtime-keyed cache
   (`~/.cache/dedupe-hashes.tsv`) so repeat runs are fast
4. Group by hash, sort biggest waste first
5. Trash moves re-verify both hashes at move time — the cache is never
   trusted for the deletion decision

## License

MIT — see [LICENSE](LICENSE).
