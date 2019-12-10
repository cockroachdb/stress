// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// The stress utility is intended for catching of episodic failures. It runs a
// given process in parallel in a loop and collects any failures.
//
// Example usage:
//
// 	$ stress ./mypkg.test -test.run=TestSomething
//
// You can also specify a number of parallel processes with -p flag; instruct
// the utility to not kill hanged processes for gdb attach; or specify the
// failure output you are looking for (if you want to ignore some other episodic
// failures).
//
// Additional flags are available to tune how many iterations to run (either by
// time or count). In either case, however, a successful stress run will always
// have completed at least one iteration, and in-flight work will be allowed to
// complete before returning.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"runtime"
	"syscall"
	"time"
)

type regexpVar struct {
	*regexp.Regexp
}

func (r *regexpVar) Set(s string) error {
	if s == "" {
		return nil
	}
	re, err := regexp.Compile(s)
	if err != nil {
		return err
	}
	*r = regexpVar{re}
	return nil
}

func (r *regexpVar) String() string {
	if r.Regexp == nil {
		return ""
	}
	return r.Regexp.String()
}

type flagT struct {
	p               int
	timeout         time.Duration
	kill            bool
	failure, ignore regexpVar
	maxtime         time.Duration
	maxruns         int
	maxfails        int
	stderr          bool

	// Only for testing.
	timeless bool
}

var opts flagT

func init() {
	flag.IntVar(&opts.p, "p", runtime.NumCPU(), "run `N` processes in parallel")
	flag.DurationVar(&opts.timeout, "timeout", 0, "timeout each process after `duration`")
	flag.BoolVar(&opts.kill, "kill", true, "kill timed out processes if true, "+
		"otherwise just print pid (to attach with gdb) and do not fail stress")
	flag.Var(&opts.failure, "failure", "fail only if output matches `regexp`")
	flag.Var(&opts.ignore, "ignore", "ignore failure if output matches `regexp`")
	flag.DurationVar(&opts.maxtime, "maxtime", 0, "drain after duration has elapsed")
	flag.IntVar(&opts.maxruns, "maxruns", 0, "maximum number of runs")
	flag.IntVar(&opts.maxfails, "maxfails", 1, "maximum number of failures")
	flag.BoolVar(&opts.stderr, "stderr", true, "output failures to STDERR instead of to a temp file")
}

func work(ctx context.Context, opts flagT) []byte {
	var cmd *exec.Cmd

	if opts.timeout > 0 && !opts.kill {
		defer time.AfterFunc(opts.timeout, func() {
			fmt.Printf("process %v timed out\n", cmd.Process.Pid)
		}).Stop()

	}

	cmd = exec.CommandContext(ctx, flag.Args()[0], flag.Args()[1:]...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	out = append(out, fmt.Sprintf("\n\nERROR: %v\n", err)...)
	out = append(out, '\n')
	return out
}

func run(ctx context.Context, stdout, stderr io.Writer, opts flagT, work func(context.Context, flagT) []byte) (runs int, fails int) {
	if opts.p == 0 {
		fmt.Println(stderr, "can't run with concurrency zero")
		return 0, 1
	}
	startTime := time.Now()

	// Set up a context that cancels when a signal is received. On signal,
	// pending invocations of the command are aborted and stress will terminate
	// nonzero.
	c := make(chan os.Signal)
	defer close(c)
	signal.Notify(c, os.Interrupt)
	// TODO(tamird): put this behind a !windows build tag.
	signal.Notify(c, syscall.SIGHUP, syscall.SIGTERM)
	defer signal.Stop(c)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-c:
			cancel()
		case <-ctx.Done():
		}
	}()

	sem := make(chan struct{}, opts.p)
	failCh := make(chan []byte)

	onFail := func(out []byte) {
		// The context only gets canceled via a signal or when maxFailed becomes
		// true below. If it's already canceled now, then additional test output
		// is not desired (it's likely just processes that got killed).
		if ctx.Err() != nil {
			return
		}

		runs++
		if out == nil {
			return
		}
		fails++
		if maxFailed := opts.maxfails > 0 && fails >= opts.maxfails; maxFailed {
			cancel()
		}
		printFailure(stdout, stderr, out)

	}

	defer func() {
		// Wait for any in-flight work to terminate. The main goroutine is the
		// one initiating new work and it's now in this defer, so this should
		// happen soon.
		for i := 0; i < cap(sem); {
			select {
			case sem <- struct{}{}:
				i++
			case out := <-failCh:
				onFail(out)
			}
		}

		if fails == 0 {
			if err := ctx.Err(); err != nil && fails == 0 {
				fmt.Fprintln(stderr, err)
				fails++
			}
			if runs == 0 && fails == 0 {
				fmt.Fprintln(stderr, "bug: expected to run at least one iteration of stress")
				fails++
			}
		}

		fmt.Fprintf(stdout, "%v runs completed, %v failures, over %s\n",
			runs, fails, roundToSeconds(time.Since(startTime)))
	}()

	tickerDuration := 5 * time.Second
	ticker := time.NewTicker(tickerDuration).C
	if opts.timeless {
		ticker = nil // disable ticker
	}

	iters := 0
	for {
		select {
		case out := <-failCh:
			onFail(out)
		case sem <- struct{}{}:
			if ctx.Err() != nil ||
				(opts.maxruns > 0 && iters >= opts.maxruns) ||
				(opts.maxtime > 0 && iters > 0 && time.Since(startTime) >= opts.maxtime) {
				<-sem
				return
			}
			iters++
			go func(ctx context.Context) {
				defer func() { <-sem }()

				if opts.timeout > 0 && opts.kill {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, opts.timeout)
					defer cancel()
				}
				out := work(ctx, opts)

				if err := ctx.Err(); err != nil && out == nil {
					out = []byte(ctx.Err().Error())
				}
				if (opts.failure.Regexp != nil && !opts.failure.Match(out)) || (opts.ignore.Regexp != nil && opts.ignore.Match(out)) {
					out = nil
				}
				failCh <- out
			}(ctx)
		case <-ticker:
			elapsed := roundToSeconds(time.Since(startTime))
			if opts.timeless {
				elapsed = 123 * time.Second
			}
			fmt.Fprintf(stdout, "%v runs so far, %v failures, over %s\n", runs, fails, elapsed)
		case <-ctx.Done(): // only triggered on signal
			return
		}
	}
}

func main() {
	if !flag.Parsed() {
		flag.Parse()
	}
	_, fails := run(context.Background(), os.Stdout, os.Stderr, opts, work)
	if fails > 0 {
		fmt.Println("FAIL")
		os.Exit(1)
	} else {
		fmt.Println("SUCCESS")
	}
}

func roundToSeconds(d time.Duration) time.Duration {
	return time.Duration(d.Seconds()+0.5) * time.Second
}

func printFailure(stdout, stderr io.Writer, out []byte) {
	if !opts.stderr {
		err := func() error {
			f, err := ioutil.TempFile("", "go-stress")
			if err != nil {
				return fmt.Errorf("failed to create temp file: %v", err)
			}
			if _, err := f.Write(out); err != nil {
				return fmt.Errorf("failed to write temp file: %v", err)
			}
			if err := f.Close(); err != nil {
				return fmt.Errorf("failed to close temp file: %v", err)
			}
			if len(out) > 2<<10 {
				out = out[:2<<10]
			}
			fmt.Fprintf(stdout, "\n%s\n%s\n", f.Name(), out)
			return nil
		}()
		if err == nil {
			return
		}
		fmt.Fprintln(stderr, err)
		// Failed to create file, fallthrough to stderr below.
	}
	if out != nil {
		fmt.Fprintf(stderr, "\n%s\n", out)
	}
}
