// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjautil

func editDistance(s1, s2 string, max int) int {
	// The algorithm implemented below is the "classic"
	// dynamic-programming algorithm for computing the Levenshtein
	// distance, which is described here:
	//
	//   http://en.wikipedia.org/wiki/Levenshtein_distance
	//
	// Although the algorithm is typically described using an m x n
	// array, only one row plus one element are used at a time, so this
	// implementation just keeps one vector for the row.  To update one entry,
	// only the entries to the left, top, and top-left are needed.  The left
	// entry is in row[x-1], the top entry is what's in row[x] from the last
	// iteration, and the top-left entry is stored in previous.
	m := len(s1)
	n := len(s2)

	row := make([]int, n+1)
	for i := 1; i <= n; i++ {
		row[i] = i
	}

	for y := 1; y <= m; y++ {
		row[0] = y
		bestThisRow := row[0]
		previous := y - 1
		for x := 1; x <= n; x++ {
			oldRow := row[x]
			p := previous
			if s1[y-1] != s2[x-1] {
				p++
			}
			row[x] = min(p, min(row[x-1], row[x])+1)
			previous = oldRow
			bestThisRow = min(bestThisRow, row[x])
		}
		if max > 0 && bestThisRow > max {
			return max + 1
		}
	}
	return row[n]
}
