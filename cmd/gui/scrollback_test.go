package main

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/egoist/mygo/plugins/terminal"
)

// Scrollback tests (see Settings.scrollbackBytes): the "max
// scrollback lines" setting is a row count, as in the WebUI
// (xterm.js scrollback), but the plugin's Scrollback is a byte
// budget over stored cells. The old conversion (lines * 160)
// starved wide terminals: at 148 columns, 10000 lines only
// retained ~890 rows. The conversion now charges per cell at the
// shown width, and these tests pin that a terminal built with the
// converted budget retains the set number of lines.

// TestScrollbackBytes pins the conversion: per-cell charge at the
// shown width, a 160-column floor (the budget is fixed at creation
// while the window can widen later), and the 256 MiB cap.
func TestScrollbackBytes(t *testing.T) {
	cases := []struct {
		lines, cols, want int
	}{
		{10000, 148, 10000 * 160 * 16}, // floor: 148 < 160
		{10000, 80, 10000 * 160 * 16},  // floor: 80 < 160
		{10000, 200, 10000 * 200 * 16},
		{1000, 148, 1000 * 160 * 16},
		{100000, 160, 100000 * 160 * 16}, // 256,000,000: just under the cap
		{100000, 240, 256 << 20},         // capped
		{0, 148, 10 << 20},               // unset: plugin default
		{-5, 148, 10 << 20},
	}
	for _, tc := range cases {
		s := Settings{Scrollback: tc.lines}
		if got := s.scrollbackBytes(tc.cols); got != tc.want {
			t.Errorf("Scrollback %d lines @ %d cols: scrollbackBytes = %d, want %d",
				tc.lines, tc.cols, got, tc.want)
		}
	}
}

// scrollbackTerm builds a Conn-mode terminal with the given byte
// scrollback budget, resized to cols x 24, before any output.
// Terminals are created BEFORE any HOME redirect: terminal.New
// loads libghostty-vt from the user cache, and a temp HOME would
// make it try to download the library instead.
func scrollbackTerm(t *testing.T, budget, cols int) *terminal.Terminal {
	t.Helper()
	term, err := terminal.New(terminal.Options{
		Conn:       newPortConn(&fakeSender{}, "scrollback"),
		Scrollback: budget,
	})
	if err != nil {
		t.Fatalf("terminal.New: %v", err)
	}
	t.Cleanup(func() { term.Close() })
	term.Resize(cols, 24)
	return term
}

// feedNumbered feeds n numbered full-width lines into the
// terminal. Feed is synchronous: once it returns, the parser has
// processed everything.
func feedNumbered(t *testing.T, term *terminal.Terminal, n, cols int) {
	t.Helper()
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "L%05d %s\r\n", i, strings.Repeat("x", cols-7))
	}
	term.Feed([]byte(b.String()))
}

// retainedLines counts the distinct numbered lines still present
// in screen + scrollback text.
func retainedLines(term *terminal.Terminal) int {
	text := term.Text()
	count := 0
	for _, line := range strings.Split(text, "\n") {
		if len(line) >= 6 && line[0] == 'L' {
			if _, err := strconv.Atoi(line[1:6]); err == nil {
				count++
			}
		}
	}
	return count
}

// TestScrollbackRetainsSetLines is the end-to-end check: a
// terminal built with the converted budget retains the set number
// of lines, at narrow and wide widths. With the old conversion
// (10000 lines -> 1,600,000 bytes) the 148-column case retained
// only 894 of 10000 lines.
func TestScrollbackRetainsSetLines(t *testing.T) {
	cases := []struct {
		lines, cols int
	}{
		{1000, 148},
		{10000, 80},
		{10000, 148},
		{10000, 200},
	}
	for _, tc := range cases {
		budget := Settings{Scrollback: tc.lines}.scrollbackBytes(tc.cols)
		term := scrollbackTerm(t, budget, tc.cols)
		feedNumbered(t, term, tc.lines, tc.cols)
		if got := retainedLines(term); got != tc.lines {
			t.Errorf("%d lines @ %d cols (budget %d): retained %d lines, want %d",
				tc.lines, tc.cols, budget, got, tc.lines)
		}
	}
}
