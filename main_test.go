// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"testing"
)

func TestParseParallel(t *testing.T) {
	tests := []struct {
		val      string
		numCPUs  int
		expected int // -1 if error is expected
	}{
		{val: "foo", numCPUs: 8, expected: -1},
		{val: "0", numCPUs: 8, expected: -1},
		{val: "-1", numCPUs: 8, expected: -1},
		{val: "0%", numCPUs: 8, expected: -1},
		{val: "-1%", numCPUs: 8, expected: -1},

		{val: "1", numCPUs: 8, expected: 1},
		{val: "1", numCPUs: 10, expected: 1},
		{val: "50", numCPUs: 12, expected: 50},

		{val: "1%", numCPUs: 8, expected: 1},
		{val: "10%", numCPUs: 8, expected: 1},
		{val: "50%", numCPUs: 8, expected: 4},
		{val: "90%", numCPUs: 8, expected: 7},
		{val: "200%", numCPUs: 8, expected: 16},

		{val: "1%", numCPUs: 1, expected: 1},
		{val: "10%", numCPUs: 1, expected: 1},
		{val: "50%", numCPUs: 1, expected: 1},
		{val: "90%", numCPUs: 1, expected: 1},
		{val: "200%", numCPUs: 1, expected: 2},

		{val: "1%", numCPUs: 2, expected: 1},
		{val: "10%", numCPUs: 2, expected: 1},
		{val: "50%", numCPUs: 2, expected: 1},
		{val: "90%", numCPUs: 2, expected: 2},
		{val: "200%", numCPUs: 2, expected: 4},

		{val: "1%", numCPUs: 24, expected: 1},
		{val: "10%", numCPUs: 24, expected: 2},
		{val: "50%", numCPUs: 24, expected: 12},
		{val: "90%", numCPUs: 24, expected: 22},
		{val: "200%", numCPUs: 24, expected: 48},
	}

	for _, tc := range tests {
		r, err := parseParallelism(tc.val, tc.numCPUs)
		if err != nil {
			if tc.expected != -1 {
				t.Fatalf("val %q, numCPUs %d: unexpected error: %v", tc.val, tc.numCPUs, err)
			}
		} else if r != tc.expected {
			t.Fatalf("val %q, numCPUs %d: expected %d, got %d", tc.val, tc.numCPUs, tc.expected, r)
		}
	}
}
