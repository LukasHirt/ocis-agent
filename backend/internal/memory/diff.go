package memory

import "strings"

// maxLCSCells bounds the line-diff table; larger inputs are shown as a full
// replacement instead of an exact diff.
const maxLCSCells = 1_000_000

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// Diff returns the line changes from before to after: removed lines as
// "-line", added lines as "+line", unchanged lines are not listed. It is empty
// when the texts are equal. It is what the activity feed shows for a memory
// write.
func Diff(before, after string) string {
	if before == after {
		return ""
	}
	a, b := splitLines(before), splitLines(after)
	p := 0
	for p < len(a) && p < len(b) && a[p] == b[p] {
		p++
	}
	s := 0
	for s < len(a)-p && s < len(b)-p && a[len(a)-1-s] == b[len(b)-1-s] {
		s++
	}
	a, b = a[p:len(a)-s], b[p:len(b)-s]
	if len(a) == 0 && len(b) == 0 {
		return "~ trailing newline changed\n"
	}

	var out strings.Builder
	del := func(l string) { out.WriteString("-" + l + "\n") }
	add := func(l string) { out.WriteString("+" + l + "\n") }
	if len(a)*len(b) > maxLCSCells {
		for _, l := range a {
			del(l)
		}
		for _, l := range b {
			add(l)
		}
		return out.String()
	}

	n, m := len(a), len(b)
	dp := make([][]int32, n+1)
	for i := range dp {
		dp[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else {
				dp[i][j] = max(dp[i+1][j], dp[i][j+1])
			}
		}
	}
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			del(a[i])
			i++
		default:
			add(b[j])
			j++
		}
	}
	for ; i < n; i++ {
		del(a[i])
	}
	for ; j < m; j++ {
		add(b[j])
	}
	return out.String()
}
