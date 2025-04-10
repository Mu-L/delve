package gdbserial_test

import (
	"errors"
	"flag"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/go-delve/delve/pkg/logflags"
	"github.com/go-delve/delve/pkg/proc"
	"github.com/go-delve/delve/pkg/proc/gdbserial"
	protest "github.com/go-delve/delve/pkg/proc/test"
)

func TestMain(m *testing.M) {
	var logConf string
	flag.StringVar(&logConf, "log", "", "configures logging")
	flag.Parse()
	logflags.Setup(logConf != "", logConf, "")
	protest.RunTestsWithFixtures(m)
}

func withTestRecording(name string, t testing.TB, fn func(grp *proc.TargetGroup, fixture protest.Fixture)) {
	fixture := protest.BuildFixture(t, name, 0)
	protest.MustHaveRecordingAllowed(t)
	if path, _ := exec.LookPath("rr"); path == "" {
		t.Skip("test skipped, rr not found")
	}
	t.Log("recording")
	grp, tracedir, err := gdbserial.RecordAndReplay([]string{fixture.Path}, ".", true, []string{}, "", proc.OutputRedirect{}, proc.OutputRedirect{})
	if err != nil {
		t.Fatal("Launch():", err)
	}
	t.Logf("replaying %q", tracedir)

	defer grp.Detach(true)

	fn(grp, fixture)
}

func assertNoError(err error, t testing.TB, s string) {
	if err != nil {
		_, file, line, _ := runtime.Caller(1)
		fname := filepath.Base(file)
		t.Fatalf("failed assertion at %s:%d: %s - %s\n", fname, line, s, err)
	}
}

func setFunctionBreakpoint(p *proc.Target, t *testing.T, fname string) *proc.Breakpoint {
	_, f, l, _ := runtime.Caller(1)
	f = filepath.Base(f)

	addrs, err := proc.FindFunctionLocation(p, fname, 0)
	if err != nil {
		t.Fatalf("%s:%d: FindFunctionLocation(%s): %v", f, l, fname, err)
	}
	if len(addrs) != 1 {
		t.Fatalf("%s:%d: setFunctionBreakpoint(%s): too many results %v", f, l, fname, addrs)
	}
	bp, err := p.SetBreakpoint(0, addrs[0], proc.UserBreakpoint, nil)
	if err != nil {
		t.Fatalf("%s:%d: FindFunctionLocation(%s): %v", f, l, fname, err)
	}
	return bp
}

func TestRestartAfterExit(t *testing.T) {
	protest.AllowRecording(t)
	withTestRecording("testnextprog", t, func(grp *proc.TargetGroup, fixture protest.Fixture) {
		p := grp.Selected
		setFunctionBreakpoint(p, t, "main.main")
		assertNoError(grp.Continue(), t, "Continue")
		loc, err := proc.ThreadLocation(p.CurrentThread())
		assertNoError(err, t, "CurrentThread().Location()")
		err = grp.Continue()
		if !errors.As(err, &proc.ErrProcessExited{}) {
			t.Fatalf("program did not exit: %v", err)
		}

		assertNoError(grp.Restart(""), t, "Restart")

		assertNoError(grp.Continue(), t, "Continue (after restart)")
		loc2, err := proc.ThreadLocation(p.CurrentThread())
		assertNoError(err, t, "CurrentThread().Location() (after restart)")
		if loc2.Line != loc.Line {
			t.Fatalf("stopped at %d (expected %d)", loc2.Line, loc.Line)
		}
		err = grp.Continue()
		if !errors.As(err, &proc.ErrProcessExited{}) {
			t.Fatalf("program did not exit (after exit): %v", err)
		}
	})
}

func TestRestartDuringStop(t *testing.T) {
	protest.AllowRecording(t)
	withTestRecording("testnextprog", t, func(grp *proc.TargetGroup, fixture protest.Fixture) {
		p := grp.Selected
		setFunctionBreakpoint(p, t, "main.main")
		assertNoError(grp.Continue(), t, "Continue")
		loc, err := proc.ThreadLocation(p.CurrentThread())
		assertNoError(err, t, "CurrentThread().Location()")

		assertNoError(grp.Restart(""), t, "Restart")

		assertNoError(grp.Continue(), t, "Continue (after restart)")
		loc2, err := proc.ThreadLocation(p.CurrentThread())
		assertNoError(err, t, "CurrentThread().Location() (after restart)")
		if loc2.Line != loc.Line {
			t.Fatalf("stopped at %d (expected %d)", loc2.Line, loc.Line)
		}
		err = grp.Continue()
		if !errors.As(err, &proc.ErrProcessExited{}) {
			t.Fatalf("program did not exit (after exit): %v", err)
		}
	})
}

func setFileBreakpoint(p *proc.Target, t *testing.T, fixture protest.Fixture, lineno int) *proc.Breakpoint {
	_, f, l, _ := runtime.Caller(1)
	f = filepath.Base(f)

	addrs, err := proc.FindFileLocation(p, fixture.Source, lineno)
	if err != nil {
		t.Fatalf("%s:%d: FindFileLocation(%s, %d): %v", f, l, fixture.Source, lineno, err)
	}
	if len(addrs) != 1 {
		t.Fatalf("%s:%d: setFileLineBreakpoint(%s, %d): too many results %v", f, l, fixture.Source, lineno, addrs)
	}
	bp, err := p.SetBreakpoint(int(addrs[0]), addrs[0], proc.UserBreakpoint, nil)
	if err != nil {
		t.Fatalf("%s:%d: SetBreakpoint: %v", f, l, err)
	}
	return bp
}

func TestReverseBreakpointCounts(t *testing.T) {
	protest.AllowRecording(t)
	withTestRecording("bpcountstest", t, func(grp *proc.TargetGroup, fixture protest.Fixture) {
		p := grp.Selected
		endbp := setFileBreakpoint(p, t, fixture, 28)
		assertNoError(grp.Continue(), t, "Continue()")
		loc, _ := proc.ThreadLocation(p.CurrentThread())
		if loc.PC != endbp.Addr {
			t.Fatalf("did not reach end of main.main function: %s:%d (%#x)", loc.File, loc.Line, loc.PC)
		}

		p.ClearBreakpoint(endbp.Addr)
		assertNoError(grp.ChangeDirection(proc.Backward), t, "Switching to backward direction")
		bp := setFileBreakpoint(p, t, fixture, 12)
		startbp := setFileBreakpoint(p, t, fixture, 20)

	countLoop:
		for {
			assertNoError(grp.Continue(), t, "Continue()")
			loc, _ := proc.ThreadLocation(p.CurrentThread())
			switch loc.PC {
			case startbp.Addr:
				break countLoop
			case bp.Addr:
				// ok
			default:
				t.Fatalf("unexpected stop location %s:%d %#x", loc.File, loc.Line, loc.PC)
			}
		}

		t.Logf("TotalHitCount: %d", bp.Logical.TotalHitCount)
		if bp.Logical.TotalHitCount != 200 {
			t.Fatalf("Wrong TotalHitCount for the breakpoint (%d)", bp.Logical.TotalHitCount)
		}

		if len(bp.Logical.HitCount) != 2 {
			t.Fatalf("Wrong number of goroutines for breakpoint (%d)", len(bp.Logical.HitCount))
		}

		for _, v := range bp.Logical.HitCount {
			if v != 100 {
				t.Fatalf("Wrong HitCount for breakpoint (%v)", bp.Logical.HitCount)
			}
		}
	})
}

func getPosition(grp *proc.TargetGroup, t *testing.T) (when string, loc *proc.Location) {
	var err error
	when, err = grp.When()
	assertNoError(err, t, "When")
	loc, err = proc.ThreadLocation(grp.Selected.CurrentThread())
	assertNoError(err, t, "Location")
	return
}

func TestCheckpoints(t *testing.T) {
	protest.AllowRecording(t)
	withTestRecording("continuetestprog", t, func(grp *proc.TargetGroup, fixture protest.Fixture) {
		p := grp.Selected
		// Continues until start of main.main, record output of 'when'
		bp := setFunctionBreakpoint(p, t, "main.main")
		assertNoError(grp.Continue(), t, "Continue")
		when0, loc0 := getPosition(grp, t)
		t.Logf("when0: %q (%#x) %x", when0, loc0.PC, p.CurrentThread().ThreadID())

		// Create a checkpoint and check that the list of checkpoints reflects this
		cpid, err := grp.Checkpoint("checkpoint1")
		if cpid != 1 {
			t.Errorf("unexpected checkpoint id %d", cpid)
		}
		assertNoError(err, t, "Checkpoint")
		checkpoints, err := grp.Checkpoints()
		assertNoError(err, t, "Checkpoints")
		if len(checkpoints) != 1 {
			t.Fatalf("wrong number of checkpoints %v (one expected)", checkpoints)
		}

		// Move forward with next, check that the output of 'when' changes
		assertNoError(grp.Next(), t, "First Next")
		assertNoError(grp.Next(), t, "Second Next")
		when1, loc1 := getPosition(grp, t)
		t.Logf("when1: %q (%#x) %x", when1, loc1.PC, p.CurrentThread().ThreadID())
		if loc0.PC == loc1.PC {
			t.Fatalf("next did not move process %#x", loc0.PC)
		}
		if when0 == when1 {
			t.Fatalf("output of when did not change after next: %q", when0)
		}

		// Move back to checkpoint, check that the output of 'when' is the same as
		// what it was when we set the breakpoint
		grp.Restart(fmt.Sprintf("c%d", cpid))
		g, _ := proc.FindGoroutine(p, 1)
		p.SwitchGoroutine(g)
		when2, loc2 := getPosition(grp, t)
		t.Logf("when2: %q (%#x) %x", when2, loc2.PC, p.CurrentThread().ThreadID())
		if loc2.PC != loc0.PC {
			t.Fatalf("PC address mismatch %#x != %#x", loc0.PC, loc2.PC)
		}
		if when0 != when2 {
			t.Fatalf("output of when mismatched %q != %q", when0, when2)
		}

		// Move forward with next again, check that the output of 'when' matches
		assertNoError(grp.Next(), t, "First Next")
		assertNoError(grp.Next(), t, "Second Next")
		when3, loc3 := getPosition(grp, t)
		t.Logf("when3: %q (%#x)", when3, loc3.PC)
		if loc3.PC != loc1.PC {
			t.Fatalf("PC address mismatch %#x != %#x", loc1.PC, loc3.PC)
		}
		if when3 != when1 {
			t.Fatalf("when output mismatch %q != %q", when1, when3)
		}

		// Delete breakpoint, move back to checkpoint then next twice and check
		// output of 'when' again
		err = p.ClearBreakpoint(bp.Addr)
		assertNoError(err, t, "ClearBreakpoint")
		grp.Restart(fmt.Sprintf("c%d", cpid))
		g, _ = proc.FindGoroutine(p, 1)
		p.SwitchGoroutine(g)
		assertNoError(grp.Next(), t, "First Next")
		assertNoError(grp.Next(), t, "Second Next")
		when4, loc4 := getPosition(grp, t)
		t.Logf("when4: %q (%#x)", when4, loc4.PC)
		if loc4.PC != loc1.PC {
			t.Fatalf("PC address mismatch %#x != %#x", loc1.PC, loc4.PC)
		}
		if when4 != when1 {
			t.Fatalf("when output mismatch %q != %q", when1, when4)
		}

		// Delete checkpoint, check that the list of checkpoints is updated
		assertNoError(grp.ClearCheckpoint(cpid), t, "ClearCheckpoint")
		checkpoints, err = grp.Checkpoints()
		assertNoError(err, t, "Checkpoints")
		if len(checkpoints) != 0 {
			t.Fatalf("wrong number of checkpoints %v (zero expected)", checkpoints)
		}
	})
}

func TestIssue1376(t *testing.T) {
	// Backward Continue should terminate when it encounters the start of the process.
	protest.AllowRecording(t)
	withTestRecording("continuetestprog", t, func(grp *proc.TargetGroup, fixture protest.Fixture) {
		p := grp.Selected
		bp := setFunctionBreakpoint(p, t, "main.main")
		assertNoError(grp.Continue(), t, "Continue (forward)")
		err := p.ClearBreakpoint(bp.Addr)
		assertNoError(err, t, "ClearBreakpoint")
		assertNoError(grp.ChangeDirection(proc.Backward), t, "Switching to backward direction")
		assertNoError(grp.Continue(), t, "Continue (backward)")
	})
}
