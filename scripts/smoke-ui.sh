#!/usr/bin/env bash
# Regression smoke for the hub TUI.
#
# Drives the hub's tea.Model directly via teatest (no PTY needed) and
# asserts on the rendered View() output for three scenarios:
#
#   - hub renders profile names from its locs slice
#   - typing into the filter input narrows the visible rows
#   - Tab → palette → 'n' returns actNew
#
# Anti-regression net for ui.go and hub.go: any rendering or keymap
# change that breaks the basic flows fails this script. Run from repo
# root:
#
#   ./scripts/smoke-ui.sh
#
# Exit code is 0 on green, non-zero if any TestHub* fails.

set -euo pipefail

go test -run '^TestHub' -count=1 ./cmd/claude-profiles/
echo "OK — hub TUI smoke tests pass."
