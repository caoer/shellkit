// Package ui holds shared TUI text helpers used by the inventory TUI and the
// log dashboard: visible-width measurement, padding, truncation, and ANSI
// stripping. It has no intra-repo dependencies (stdlib + lipgloss only).
package ui

import (
	"regexp"
	"strings"
)

// VisibleLen returns the number of visible runes in s, skipping ANSI escape
// sequences (so styled strings measure by what the terminal actually shows).
func VisibleLen(s string) int {
	n := 0
	inEsc := false
	for _, r := range s {
		if r == '\x1b' {
			inEsc = true
			continue
		}
		if inEsc {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEsc = false
			}
			continue
		}
		n++
	}
	return n
}

// PadTo right-pads s with spaces to a visible width of w, ANSI-aware via
// VisibleLen. Strings already at/over width are returned unchanged.
func PadTo(s string, w int) string {
	visible := VisibleLen(s)
	if visible >= w {
		return s
	}
	return s + strings.Repeat(" ", w-visible)
}

// Truncate shortens s to a byte width of w, appending "~" when it cuts. Width
// <= 3 truncates without the marker. ANSI-naive (byte-based) — used by the
// inventory table where cells are plain text.
func Truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if len(s) <= w {
		return s
	}
	if w <= 3 {
		return s[:w]
	}
	return s[:w-1] + "~"
}

// TruncLine shortens s to a width of w runes, appending "…" when it cuts.
// Rune-based — used by the dashboard for multi-byte-safe single-line clipping.
func TruncLine(s string, w int) string {
	if w <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	return string(r[:w-1]) + "…"
}

// TruncOrDash returns "-" for an empty string, else Truncate(s, w).
func TruncOrDash(s string, w int) string {
	if s == "" {
		return "-"
	}
	return Truncate(s, w)
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)

// StripANSI removes ANSI CSI escape sequences from s.
func StripANSI(s string) string {
	return ansiRe.ReplaceAllString(s, "")
}
