package config

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// registeredNames names what a registry holds, for a diagnostic about a name it
// does not: the nearest registered name when one is close to got, then
// every registered name (", registered: none" when there are none).
func registeredNames[T any](m map[string]T, got string) string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return " (none is registered)"
	}
	hint := ""
	if near := nearest(got, names); near != "" {
		hint = fmt.Sprintf("; did you mean %q?", near)
	}
	return hint + " (registered: " + strings.Join(names, ", ") + ")"
}

// unknownField is the message for a field an object does not allow: the
// nearest allowed name when one is close, else what is allowed.
func unknownField(got string, allowed []string) string {
	if near := nearest(got, allowed); near != "" {
		return fmt.Sprintf("unknown field; did you mean %q?", near)
	}
	if len(allowed) == 0 {
		return "unknown field; remove it"
	}
	sorted := slices.Clone(allowed)
	sort.Strings(sorted)
	return "unknown field; remove it (allowed: " + strings.Join(sorted, ", ") + ")"
}

// nearest is the candidate closest to got by edit distance, if it is close
// enough to be a likely typo (at most a third of got's length, at least
// one edit); "" otherwise or when two are equally close.
func nearest(got string, candidates []string) string {
	limit := max(1, len([]rune(got))/3)
	best, bestDist, tie := "", limit+1, false
	for _, c := range candidates {
		d := editDistance(strings.ToLower(got), strings.ToLower(c))
		switch {
		case d < bestDist:
			best, bestDist, tie = c, d, false
		case d == bestDist && c != best:
			tie = true
		}
	}
	if tie || bestDist > limit {
		return ""
	}
	return best
}

// editDistance is the Damerau–Levenshtein (optimal string alignment)
// distance between a and b, so a swapped pair of letters counts once.
func editDistance(a, b string) int {
	x, y := []rune(a), []rune(b)
	d := make([][]int, len(x)+1)
	for i := range d {
		d[i] = make([]int, len(y)+1)
		d[i][0] = i
	}
	for j := range d[0] {
		d[0][j] = j
	}
	for i := 1; i <= len(x); i++ {
		for j := 1; j <= len(y); j++ {
			cost := 1
			if x[i-1] == y[j-1] {
				cost = 0
			}
			d[i][j] = min(d[i-1][j]+1, d[i][j-1]+1, d[i-1][j-1]+cost)
			if i > 1 && j > 1 && x[i-1] == y[j-2] && x[i-2] == y[j-1] {
				d[i][j] = min(d[i][j], d[i-2][j-2]+1)
			}
		}
	}
	return d[len(x)][len(y)]
}

// lineColumn is the 1-based line and column of byte offset in data.
func lineColumn(data []byte, offset int64) (line, col int) {
	if offset > int64(len(data)) {
		offset = int64(len(data))
	}
	line, col = 1, 1
	for _, b := range data[:offset] {
		if b == '\n' {
			line, col = line+1, 1
		} else {
			col++
		}
	}
	return line, col
}
