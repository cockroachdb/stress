// Copyright 2019 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package main

import (
	"bytes"
	"context"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type testfile string

func (tf *testfile) String() string {
	b, err := ioutil.ReadFile((string)(*tf))
	if err != nil {
		return err.Error()
	}
	return string(b)
}

func (tf *testfile) AssertEqual(t *testing.T, actual string) bool {
	return assert.Equal(t, tf.String(), actual)
}

func (tf *testfile) MustReplace(t *testing.T, s string) {
	_ = os.MkdirAll(filepath.Dir(string(*tf)), 0755)
	if err := ioutil.WriteFile(string(*tf), []byte(s), 0644); err != nil {
		t.Fatal(err)
	}
}

type testdir string

func (td *testdir) File(name string) testfile {
	return testfile(filepath.Join(string(*td), name))
}

type state struct {
	ctx context.Context // passed to run()
	flagT

	runs, fails int   // result from run()
	calls       int32 // atomic while in run()
	work        func(_ context.Context, call int) []byte
}

type testCase struct {
	name   string
	setup  func(t *testing.T, opts *state)
	verify func(t *testing.T, opts state)
}

func TestStress(t *testing.T) {
	dir := testdir("testdata")

	tcs := []testCase{
		{
			name: "maxruns",
			setup: func(t *testing.T, opts *state) {
				opts.maxruns = 100
			},
			verify: func(t *testing.T, opts state) {
				require.EqualValues(t, 100, opts.calls)
				require.EqualValues(t, 100, opts.runs)
				require.EqualValues(t, 0, opts.fails)
			},
		},
		{
			name: "maxfails",
			setup: func(t *testing.T, opts *state) {
				opts.maxfails = 5
				opts.work = func(_ context.Context, call int) []byte {
					if call%100 == 0 {
						return []byte("boom!")
					}
					return nil
				}
			},
			verify: func(t *testing.T, opts state) {
				require.EqualValues(t, 5, opts.fails)
				require.EqualValues(t, 500, opts.calls)
				require.EqualValues(t, 500, opts.runs)
			},
		},
		{
			// Verify that when the desired number of failures has been output,
			// the remaining processes are cancelled (and not counted as runs).
			name: "maxfails-nowait",
			setup: func(t *testing.T, opts *state) {
				opts.maxfails = 1
				opts.p = 5
				opts.work = func(ctx context.Context, call int) []byte {
					if call == 5 {
						return []byte("boom!")
					}
					select {
					case <-ctx.Done():
						return nil
					case <-time.After(time.Second):
						return []byte("context not canceled")
					}
				}
			},
			verify: func(t *testing.T, opts state) {
				require.EqualValues(t, 1, opts.fails)
				require.EqualValues(t, 5, opts.calls)
				require.EqualValues(t, 1, opts.runs)
			},
		},
		{
			name: "maxtime-fail",
			setup: func(t *testing.T, opts *state) {
				opts.maxtime = time.Nanosecond
				opts.work = func(_ context.Context, call int) []byte {
					time.Sleep(time.Millisecond)
					return []byte("boom")
				}
			},
			verify: func(t *testing.T, opts state) {
				// The failure registers even though -maxtime was reached.
				require.EqualValues(t, 1, opts.fails)
				require.EqualValues(t, 1, opts.calls)
				require.EqualValues(t, 1, opts.runs)
			},
		},
		{
			name: "maxtime-pass",
			setup: func(t *testing.T, opts *state) {
				opts.maxtime = time.Nanosecond
				opts.work = func(ctx context.Context, call int) []byte {
					time.Sleep(time.Millisecond)
					var out []byte
					if err := ctx.Err(); err != nil {
						// Should not enter this branch. -maxtime should not
						// cancel the context on us.
						out = []byte(err.Error())
					}
					return out
				}
			},
			verify: func(t *testing.T, opts state) {
				// The success registers even though -maxtime was reached.
				require.EqualValues(t, 0, opts.fails)
				require.EqualValues(t, 1, opts.calls)
				require.EqualValues(t, 1, opts.runs)
			},
		},
		{
			name: "ignore",
			setup: func(t *testing.T, opts *state) {
				opts.maxruns = 500
				opts.ignore.Regexp = regexp.MustCompile("bang")
				opts.work = func(_ context.Context, call int) []byte {
					if call%100 == 0 {
						return []byte("boom!")
					}
					return []byte("bang")
				}
			},
			verify: func(t *testing.T, opts state) {
				require.EqualValues(t, 5, opts.fails)
				require.EqualValues(t, 500, opts.calls)
				require.EqualValues(t, 500, opts.runs)
			},
		},
		{
			name: "failure",
			setup: func(t *testing.T, opts *state) {
				opts.maxruns = 500
				opts.failure.Regexp = regexp.MustCompile("bang")
				opts.work = func(_ context.Context, call int) []byte {
					if call%100 == 0 {
						return []byte("bang")
					}
					return []byte("boom!")
				}
			},
			verify: func(t *testing.T, opts state) {
				require.EqualValues(t, 5, opts.fails)
				require.EqualValues(t, 500, opts.calls)
				require.EqualValues(t, 500, opts.runs)
			},
		},
		{
			name: "timeout-kill",
			setup: func(t *testing.T, opts *state) {
				opts.maxfails = 1
				opts.timeout = 10 * time.Millisecond
				opts.kill = true
				opts.work = func(ctx context.Context, call int) []byte {
					select {
					case <-ctx.Done():
						// Returning nil verifies that the stress runner realizes
						// that there was a timeout even when no explicit error is
						// returned.
						return nil
					case <-time.After(time.Second):
						return []byte("context not canceled")
					}
				}
			},
			verify: func(t *testing.T, opts state) {
				require.EqualValues(t, 1, opts.fails)
				require.EqualValues(t, 1, opts.runs)
				require.EqualValues(t, 1, opts.calls)
			},
		},
		{
			name: "timeout-nokill",
			setup: func(t *testing.T, opts *state) {
				opts.maxfails = 1
				opts.maxruns = 10
				opts.timeout = time.Millisecond
				opts.work = func(ctx context.Context, call int) []byte {
					if call == 5 {
						time.Sleep(10 * time.Millisecond)
					}
					return nil
				}
			},
			verify: func(t *testing.T, opts state) {
				// At least one work invocation will have timed out (perhaps more)
				// but since we specified -kill=false, this won't have any effect
				// on the outcome.
				require.EqualValues(t, 0, opts.fails)
				require.EqualValues(t, 10, opts.runs)
				require.EqualValues(t, 10, opts.calls)
			},
		},
		{
			name: "parallelism",
			setup: func(t *testing.T, opts *state) {
				opts.maxruns = 1000
				opts.p = 24

				// Workers can proceed when this atomic var hits >= opts.p
				cur := int32(0)

				opts.work = func(ctx context.Context, call int) []byte {
					atomic.AddInt32(&cur, 1)
					for ; int(atomic.LoadInt32(&cur)) < opts.p; time.Sleep(time.Millisecond) {
					}
					// If this line is reached, opts.p workers were at some point
					// running in concurrently.
					return nil
				}
			},
			verify: func(t *testing.T, opts state) {
				require.EqualValues(t, 0, opts.fails)
				require.EqualValues(t, 1000, opts.runs)
				require.EqualValues(t, 1000, opts.calls)
			},
		},
		{
			// Verify that when the main context gets canceled (i.e. on stress
			// being terminated) work tears down quickly. This tests the output
			// more than the unsurprising fact that the cancellation propagates.
			name: "signal-mock",
			setup: func(t *testing.T, opts *state) {
				opts.p = 10
				var cancel func()
				opts.ctx, cancel = context.WithCancel(context.Background())
				opts.work = func(ctx context.Context, call int) []byte {
					if call == 10 {
						cancel() // simulate SIGTERM or similar signal
					}
					select {
					case <-ctx.Done():
					case <-time.After(time.Second):
						return []byte("context not canceled")
					}
					return []byte("boom")
				}
			},
			verify: func(t *testing.T, opts state) {
				// The failures aren't counted since the context got canceled
				// early, but the context cancellation itself counts as a
				// failure.
				require.EqualValues(t, 1, opts.fails)
				// NB: we can't say much about how many runs
			},
		},
	}

	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			f := dir.File(tc.name)
			opts := state{ctx: context.Background(), flagT: flagT{p: 1, stderr: true, timeless: true}}
			tc.setup(t, &opts)

			work := func(ctx context.Context, _ flagT) []byte {
				n := atomic.AddInt32(&opts.calls, 1)
				if opts.work != nil {
					return opts.work(ctx, int(n))
				}
				return nil
			}
			out := &bytes.Buffer{}
			opts.runs, opts.fails = run(opts.ctx, out, out, opts.flagT, work)
			tc.verify(t, opts)
			if act := out.String(); !f.AssertEqual(t, act) {
				f.MustReplace(t, act)
			}
		})
	}

}
