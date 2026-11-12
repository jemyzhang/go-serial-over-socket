package main

import (
	"context"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// listMonoFontFamilies enumerates the families fontconfig classifies
// as monospaced (spacing=mono) — the ones a terminal can use. This is
// a small local copy of the previous webview client's approach:
// same fc-list query, same parse. Any
// failure returns nil and the settings font field falls back to free
// text input. Per Jemy's settled direction, when the list is
// available it is shown flat, with no preset group.
func listMonoFontFamilies() []string {
	if fams := fcListFamilies(":spacing=mono"); len(fams) > 0 {
		return fams
	}
	return fcListFamilies("")
}

func fcListFamilies(pattern string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var args []string
	if pattern != "" {
		args = append(args, pattern)
	}
	args = append(args, "family")
	out, err := exec.CommandContext(ctx, "fc-list", args...).Output()
	if err != nil {
		return nil
	}
	return parseFcListFamilies(string(out))
}

// parseFcListFamilies turns `fc-list ... family` output into a
// sorted, de-duplicated family list. fc-list prints one line per
// font file with that font's families comma-joined (a family plus
// its aliases), so both levels are split.
func parseFcListFamilies(out string) []string {
	seen := map[string]bool{}
	var fams []string
	for _, line := range strings.Split(out, "\n") {
		for _, fam := range strings.Split(line, ",") {
			fam = strings.TrimSpace(fam)
			if fam == "" || seen[fam] {
				continue
			}
			seen[fam] = true
			fams = append(fams, fam)
		}
	}
	sort.Strings(fams)
	return fams
}
