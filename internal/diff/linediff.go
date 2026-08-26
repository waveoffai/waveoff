// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package diff

import "strings"

// lineDiffCounts reports how many lines were added and removed between two
// prompt bodies, via a longest-common-subsequence. Prompts are small enough
// that the quadratic table is not worth avoiding, and an exact count is worth
// more than an approximate one when it is the only signal an operator has about
// the size of a prompt change.
func lineDiffCounts(a, b string) (added, removed int) {
	la := splitLines(a)
	lb := splitLines(b)

	// lcs[i][j] = length of the longest common subsequence of la[i:] and lb[j:].
	lcs := make([][]int, len(la)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(lb)+1)
	}
	for i := len(la) - 1; i >= 0; i-- {
		for j := len(lb) - 1; j >= 0; j-- {
			if la[i] == lb[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	common := lcs[0][0]
	return len(lb) - common, len(la) - common
}

func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
