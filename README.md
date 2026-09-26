# lc

`lc` lists local coding-agent sessions for the Git repository you are in and lets you browse, preview, and resume them. It reads local session stores only; no transcript leaves your machine.

## Install

With Go 1.25 or newer:

```sh
go install github.com/HACKE-RC/lc@latest
```

From a checkout:

```sh
sh scripts/install.sh      # go install into $(go env GOBIN) or $(go env GOPATH)/bin
go build -o lc .           # or just build a local binary
```

Make sure the Go bin directory is on your `PATH`, then run `lc`. `lc --version` prints the version.

Versions up to 0.2.0 were a Python package on PyPI (`list-coding-agents`). From 0.3.0, `lc` is a single Go binary. The flags and output are the same.

## Quick start

```sh
# Sessions for the current repository
lc

# Browse another directory
lc -I ~/projects/lc

# Restrict to agents; an existing directory can be positional too
lc codex claude
lc ~/projects/lc codex

# Every repository or machine-readable output
lc --all
lc --json
```

By default, `lc` shows at most 40 matching sessions. Use `--limit 0` for all of them. The listing is colored on a terminal and plain when piped or when `NO_COLOR` is set.

## Filters

```sh
lc --agent codex --agent claude
lc -X gemini
lc --only-folder api
lc --except-folder 'worktrees/*'
lc --ids
lc --no-size
```

`--only-folder` and `--except-folder` accept a subdirectory, absolute path, glob, or bare directory name. `lc --help` has the full option list.

## Interactive browser

`lc -I` opens a full-screen browser built with [Bubble Tea](https://github.com/charmbracelet/bubbletea), [Lip Gloss](https://github.com/charmbracelet/lipgloss), and [Glamour](https://github.com/charmbracelet/glamour). It shows a spinner while it scans the session stores. The left pane lists sessions with their title, agent, directory, and age. The right pane shows the selected session's latest turns as rendered Markdown, with syntax-highlighted code blocks. Colors use Catppuccin: Mocha on dark terminals and Latte on light ones. Every agent has its own color; Claude Code uses its orange accent. Terminals narrower than 90 columns show only the list, but `o` still opens the preview.

| Key or action | Result |
| --- | --- |
| `j` / `k`, arrows, mouse wheel | Move through sessions |
| Click a session | Select it |
| `gg` / `G`, Home / End | First / last session |
| `Ctrl-D` / `Ctrl-U` | Move by half a page |
| Page Up / Down, `Ctrl-B` / `Ctrl-F`, Space | Move by a page |
| `J` / `K`, mouse wheel over the preview | Scroll the preview |
| `o` | Open the preview full-screen (see below) |
| `/` | Filter by agent, directory, or title. Every word you type must match. `Enter` keeps the filter and `Esc` clears it; `/` again edits a kept filter |
| Drag the gap between the panes | Resize them |
| `[` / `]`, Left / Right | Resize the panes without a mouse |
| `h`, `u`, `H` | Hide a directory, undo, or restore all |
| `i` | Toggle IDs |
| `Enter` | Resume the session when the agent supports it |
| `p` | Print the selected transcript path |
| `?` | Show all key bindings |
| `q`, `Esc`, or `Ctrl-C` | Quit. With a kept filter, `Esc` clears it first |

The selected session's age and transcript size appear at the bottom right. If you hid directories, `lc` prints the `-x` flags that keep them hidden when you quit. Resuming starts the agent in the session's working directory.

### Reading a preview

`o` opens the selected session's preview full-screen, where it scrolls like a pager.

| Key | Result |
| --- | --- |
| `j` / `k`, arrows, mouse wheel | Scroll by a line |
| `Ctrl-D` / `Ctrl-U`, Page Up / Down, `Ctrl-B` / `Ctrl-F`, Space | Scroll by half a page or a page |
| `gg` / `G`, Home / End | Top / bottom |
| `/` | Search, case-insensitive. The view jumps to matches as you type. `Enter` keeps the search; `Esc` cancels it |
| `n` / `N` | Next / previous match, wrapping around. The current match is orange, the others are shaded |
| `Esc` | Clear the search; a second `Esc` goes back to the list |
| `o`, `q` | Back to the list |
| `Enter`, `p` | Resume the session or print its path |
| `?` | Show all reader keys |
| `Ctrl-C` | Quit |

The search bar at the bottom shows which match you're on, or "no matches". Matches can't span a wrapped line.

## Names and performance

Codex, Claude Code, pi, and omp native names take precedence. Other stores fall back to the first useful user prompt. Generic startup titles and injected context are ignored. Codex threads without a typed prompt are named after their goal objective, or shown as `subagent: <path> (<nickname>)` when a parent agent spawned them. Previews read both the older Codex event log and the `item_completed` records that Codex 0.150+ writes.

Codex rollouts can add up. `lc` reads compact metadata first and only opens a full transcript when it needs a title. It keeps a disposable local cache at:

```text
$XDG_CACHE_HOME/lc/codex-sessions.json
```

If `XDG_CACHE_HOME` is unset, the path is `~/.cache/lc/codex-sessions.json`. The cache is validated with transcript timestamp and size. Delete it whenever you want; `lc` will rebuild it.

## Supported stores

Claude Code, Codex, Droid, OpenCode, Cursor, GitHub Copilot, Grok, Kimi, Gemini, pi, and omp (oh-my-pi, including `$XDG_DATA_HOME/omp`). omp subagent transcripts are not listed. Missing stores are ignored. Cursor transcripts have no preview; press `p` for the path.

## Development

```sh
go vet ./...
go test ./...
```

The tests cover title fallbacks, preview branch-following, wrapper stripping, folder patterns, timestamp parsing, and the Codex cache contract.
