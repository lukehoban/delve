package proc

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	protest "github.com/derekparker/delve/proc/test"
)

func init() {
	runtime.GOMAXPROCS(4)
	os.Setenv("GOMAXPROCS", "4")
}

func TestMain(m *testing.M) {
	os.Exit(protest.RunTestsWithFixtures(m))
}

func withTestProcess(name string, t *testing.T, fn func(p *Process, fixture protest.Fixture)) {
	fixture := protest.BuildFixture(name)
	p, err := Launch([]string{fixture.Path})
	if err != nil {
		t.Fatal("Launch():", err)
	}

	defer func() {
		p.Halt()
		p.Kill()
	}()

	fn(p, fixture)
}

func getRegisters(p *Process, t *testing.T) Registers {
	regs, err := p.Registers()
	if err != nil {
		t.Fatal("Registers():", err)
	}

	return regs
}

func dataAtAddr(thread *Thread, addr uint64) ([]byte, error) {
	return thread.readMemory(uintptr(addr), 1)
}

func assertNoError(err error, t *testing.T, s string) {
	if err != nil {
		_, file, line, _ := runtime.Caller(1)
		fname := filepath.Base(file)
		t.Fatalf("failed assertion at %s:%d: %s - %s\n", fname, line, s, err)
	}
}

func currentPC(p *Process, t *testing.T) uint64 {
	pc, err := p.PC()
	if err != nil {
		t.Fatal(err)
	}

	return pc
}

func currentLineNumber(p *Process, t *testing.T) (string, int) {
	pc := currentPC(p, t)
	f, l, _ := p.goSymTable.PCToLine(pc)

	return f, l
}

func TestExit(t *testing.T) {
	withTestProcess("continuetestprog", t, func(p *Process, fixture protest.Fixture) {
		err := p.Continue()
		pe, ok := err.(ProcessExitedError)
		if !ok {
			t.Fatalf("Continue() returned unexpected error type %s", err)
		}
		if pe.Status != 0 {
			t.Errorf("Unexpected error status: %d", pe.Status)
		}
		if pe.Pid != p.Pid {
			t.Errorf("Unexpected process id: %d", pe.Pid)
		}
	})
}

func setFunctionBreakpoint(p *Process, fname string) (*Breakpoint, error) {
	addr, err := p.FindFunctionLocation(fname, true, 0)
	if err != nil {
		return nil, err
	}
	return p.SetBreakpoint(addr)
}

func TestHalt(t *testing.T) {
	stopChan := make(chan interface{})
	withTestProcess("loopprog", t, func(p *Process, fixture protest.Fixture) {
		_, err := setFunctionBreakpoint(p, "main.loop")
		assertNoError(err, t, "SetBreakpoint")
		assertNoError(p.Continue(), t, "Continue")
		for _, th := range p.Threads {
			if th.running != false {
				t.Fatal("expected running = false for thread", th.Id)
			}
			_, err := th.Registers()
			assertNoError(err, t, "Registers")
		}
		go func() {
			for {
				if p.Running() {
					if err := p.RequestManualStop(); err != nil {
						t.Fatal(err)
					}
					stopChan <- nil
					return
				}
			}
		}()
		assertNoError(p.Continue(), t, "Continue")
		<-stopChan
		// Loop through threads and make sure they are all
		// actually stopped, err will not be nil if the process
		// is still running.
		for _, th := range p.Threads {
			if !th.Stopped() {
				t.Fatal("expected thread to be stopped, but was not")
			}
			if th.running != false {
				t.Fatal("expected running = false for thread", th.Id)
			}
			_, err := th.Registers()
			assertNoError(err, t, "Registers")
		}
	})
}

func TestStep(t *testing.T) {
	withTestProcess("testprog", t, func(p *Process, fixture protest.Fixture) {
		helloworldfunc := p.goSymTable.LookupFunc("main.helloworld")
		helloworldaddr := helloworldfunc.Entry

		_, err := p.SetBreakpoint(helloworldaddr)
		assertNoError(err, t, "SetBreakpoint()")
		assertNoError(p.Continue(), t, "Continue()")

		regs := getRegisters(p, t)
		rip := regs.PC()

		err = p.Step()
		assertNoError(err, t, "Step()")

		regs = getRegisters(p, t)
		if rip >= regs.PC() {
			t.Errorf("Expected %#v to be greater than %#v", regs.PC(), rip)
		}
	})
}

func TestBreakpoint(t *testing.T) {
	withTestProcess("testprog", t, func(p *Process, fixture protest.Fixture) {
		helloworldfunc := p.goSymTable.LookupFunc("main.helloworld")
		helloworldaddr := helloworldfunc.Entry

		bp, err := p.SetBreakpoint(helloworldaddr)
		assertNoError(err, t, "SetBreakpoint()")
		assertNoError(p.Continue(), t, "Continue()")

		pc, err := p.PC()
		if err != nil {
			t.Fatal(err)
		}

		if bp.TotalHitCount != 1 {
			t.Fatalf("Breakpoint should be hit once, got %d\n", bp.TotalHitCount)
		}

		if pc-1 != bp.Addr && pc != bp.Addr {
			f, l, _ := p.goSymTable.PCToLine(pc)
			t.Fatalf("Break not respected:\nPC:%#v %s:%d\nFN:%#v \n", pc, f, l, bp.Addr)
		}
	})
}

func TestBreakpointInSeperateGoRoutine(t *testing.T) {
	withTestProcess("testthreads", t, func(p *Process, fixture protest.Fixture) {
		fn := p.goSymTable.LookupFunc("main.anotherthread")
		if fn == nil {
			t.Fatal("No fn exists")
		}

		_, err := p.SetBreakpoint(fn.Entry)
		if err != nil {
			t.Fatal(err)
		}

		err = p.Continue()
		if err != nil {
			t.Fatal(err)
		}

		pc, err := p.PC()
		if err != nil {
			t.Fatal(err)
		}

		f, l, _ := p.goSymTable.PCToLine(pc)
		if f != "testthreads.go" && l != 8 {
			t.Fatal("Program did not hit breakpoint")
		}
	})
}

func TestBreakpointWithNonExistantFunction(t *testing.T) {
	withTestProcess("testprog", t, func(p *Process, fixture protest.Fixture) {
		_, err := p.SetBreakpoint(0)
		if err == nil {
			t.Fatal("Should not be able to break at non existant function")
		}
	})
}

func TestClearBreakpointBreakpoint(t *testing.T) {
	withTestProcess("testprog", t, func(p *Process, fixture protest.Fixture) {
		fn := p.goSymTable.LookupFunc("main.sleepytime")
		bp, err := p.SetBreakpoint(fn.Entry)
		assertNoError(err, t, "SetBreakpoint()")

		bp, err = p.ClearBreakpoint(fn.Entry)
		assertNoError(err, t, "ClearBreakpoint()")

		data, err := dataAtAddr(p.CurrentThread, bp.Addr)
		if err != nil {
			t.Fatal(err)
		}

		int3 := []byte{0xcc}
		if bytes.Equal(data, int3) {
			t.Fatalf("Breakpoint was not cleared data: %#v, int3: %#v", data, int3)
		}

		if len(p.Breakpoints) != 0 {
			t.Fatal("Breakpoint not removed internally")
		}
	})
}

type nextTest struct {
	begin, end int
}

func testnext(program string, testcases []nextTest, initialLocation string, t *testing.T) {
	withTestProcess(program, t, func(p *Process, fixture protest.Fixture) {
		bp, err := setFunctionBreakpoint(p, initialLocation)
		assertNoError(err, t, "SetBreakpoint()")
		assertNoError(p.Continue(), t, "Continue()")
		p.ClearBreakpoint(bp.Addr)
		p.CurrentThread.SetPC(bp.Addr)

		f, ln := currentLineNumber(p, t)
		for _, tc := range testcases {
			if ln != tc.begin {
				t.Fatalf("Program not stopped at correct spot expected %d was %s:%d", tc.begin, filepath.Base(f), ln)
			}

			assertNoError(p.Next(), t, "Next() returned an error")

			f, ln = currentLineNumber(p, t)
			if ln != tc.end {
				t.Fatalf("Program did not continue to correct next location expected %d was %s:%d", tc.end, filepath.Base(f), ln)
			}
		}

		if len(p.Breakpoints) != 0 {
			t.Fatal("Not all breakpoints were cleaned up", len(p.Breakpoints))
		}
	})
}

func TestNextGeneral(t *testing.T) {
	testcases := []nextTest{
		{19, 20},
		{20, 23},
		{23, 24},
		{24, 26},
		{26, 31},
		{31, 23},
		{23, 24},
		{24, 26},
		{26, 31},
		{31, 23},
		{23, 24},
		{24, 26},
		{26, 27},
		{27, 34},
	}
	testnext("testnextprog", testcases, "main.testnext", t)
}

func TestNextConcurrent(t *testing.T) {
	testcases := []nextTest{
		{9, 10},
		{10, 11},
	}
	withTestProcess("parallel_next", t, func(p *Process, fixture protest.Fixture) {
		_, err := setFunctionBreakpoint(p, "main.sayhi")
		assertNoError(err, t, "SetBreakpoint")
		assertNoError(p.Continue(), t, "Continue")
		f, ln := currentLineNumber(p, t)
		initV, err := evalVariable(p, "n")
		assertNoError(err, t, "EvalVariable")
		for _, tc := range testcases {
			g, err := p.CurrentThread.GetG()
			assertNoError(err, t, "GetG()")
			if p.SelectedGoroutine.Id != g.Id {
				t.Fatalf("SelectedGoroutine not CurrentThread's goroutine: %d %d", g.Id, p.SelectedGoroutine.Id)
			}
			if ln != tc.begin {
				t.Fatalf("Program not stopped at correct spot expected %d was %s:%d", tc.begin, filepath.Base(f), ln)
			}
			assertNoError(p.Next(), t, "Next() returned an error")
			f, ln = currentLineNumber(p, t)
			if ln != tc.end {
				t.Fatalf("Program did not continue to correct next location expected %d was %s:%d", tc.end, filepath.Base(f), ln)
			}
			v, err := evalVariable(p, "n")
			assertNoError(err, t, "EvalVariable")
			if v.Value != initV.Value {
				t.Fatal("Did not end up on same goroutine")
			}
		}
	})
}

func TestNextGoroutine(t *testing.T) {
	testcases := []nextTest{
		{47, 42},
	}
	testnext("testnextprog", testcases, "main.testgoroutine", t)
}

func TestNextFunctionReturn(t *testing.T) {
	testcases := []nextTest{
		{14, 35},
	}
	testnext("testnextprog", testcases, "main.helloworld", t)
}

func TestNextFunctionReturnDefer(t *testing.T) {
	testcases := []nextTest{
		{9, 6},
		{6, 7},
		{7, 10},
	}
	testnext("testnextdefer", testcases, "main.main", t)
}

func TestNextNetHTTP(t *testing.T) {
	testcases := []nextTest{
		{11, 12},
		{12, 13},
	}
	withTestProcess("testnextnethttp", t, func(p *Process, fixture protest.Fixture) {
		go func() {
			for !p.Running() {
				time.Sleep(50 * time.Millisecond)
			}
			// Wait for program to start listening.
			for {
				conn, err := net.Dial("tcp", ":9191")
				if err == nil {
					conn.Close()
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			http.Get("http://localhost:9191")
		}()
		if err := p.Continue(); err != nil {
			t.Fatal(err)
		}
		f, ln := currentLineNumber(p, t)
		for _, tc := range testcases {
			if ln != tc.begin {
				t.Fatalf("Program not stopped at correct spot expected %d was %s:%d", tc.begin, filepath.Base(f), ln)
			}

			assertNoError(p.Next(), t, "Next() returned an error")

			f, ln = currentLineNumber(p, t)
			if ln != tc.end {
				t.Fatalf("Program did not continue to correct next location expected %d was %s:%d", tc.end, filepath.Base(f), ln)
			}
		}
	})
}

func TestRuntimeBreakpoint(t *testing.T) {
	withTestProcess("testruntimebreakpoint", t, func(p *Process, fixture protest.Fixture) {
		err := p.Continue()
		if err != nil {
			t.Fatal(err)
		}
		pc, err := p.PC()
		if err != nil {
			t.Fatal(err)
		}
		_, l, _ := p.PCToLine(pc)
		if l != 10 {
			t.Fatal("did not respect breakpoint")
		}
	})
}

func TestFindReturnAddress(t *testing.T) {
	withTestProcess("testnextprog", t, func(p *Process, fixture protest.Fixture) {
		start, _, err := p.goSymTable.LineToPC(fixture.Source, 24)
		if err != nil {
			t.Fatal(err)
		}
		_, err = p.SetBreakpoint(start)
		if err != nil {
			t.Fatal(err)
		}
		err = p.Continue()
		if err != nil {
			t.Fatal(err)
		}
		addr, err := p.CurrentThread.ReturnAddress()
		if err != nil {
			t.Fatal(err)
		}
		_, l, _ := p.goSymTable.PCToLine(addr)
		if l != 40 {
			t.Fatalf("return address not found correctly, expected line 40")
		}
	})
}

func TestFindReturnAddressTopOfStackFn(t *testing.T) {
	withTestProcess("testreturnaddress", t, func(p *Process, fixture protest.Fixture) {
		fnName := "runtime.rt0_go"
		fn := p.goSymTable.LookupFunc(fnName)
		if fn == nil {
			t.Fatalf("could not find function %s", fnName)
		}
		if _, err := p.SetBreakpoint(fn.Entry); err != nil {
			t.Fatal(err)
		}
		if err := p.Continue(); err != nil {
			t.Fatal(err)
		}
		if _, err := p.CurrentThread.ReturnAddress(); err == nil {
			t.Fatal("expected error to be returned")
		}
	})
}

func TestSwitchThread(t *testing.T) {
	withTestProcess("testnextprog", t, func(p *Process, fixture protest.Fixture) {
		// With invalid thread id
		err := p.SwitchThread(-1)
		if err == nil {
			t.Fatal("Expected error for invalid thread id")
		}
		pc, err := p.FindFunctionLocation("main.main", true, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, err = p.SetBreakpoint(pc)
		if err != nil {
			t.Fatal(err)
		}
		err = p.Continue()
		if err != nil {
			t.Fatal(err)
		}
		var nt int
		ct := p.CurrentThread.Id
		for tid := range p.Threads {
			if tid != ct {
				nt = tid
				break
			}
		}
		if nt == 0 {
			t.Fatal("could not find thread to switch to")
		}
		// With valid thread id
		err = p.SwitchThread(nt)
		if err != nil {
			t.Fatal(err)
		}
		if p.CurrentThread.Id != nt {
			t.Fatal("Did not switch threads")
		}
	})
}

func TestCGONext(t *testing.T) {
	// Test if one can do 'next' in a cgo binary
	// On OSX with Go < 1.5 CGO is not supported due to: https://github.com/golang/go/issues/8973
	if runtime.GOOS == "darwin" && strings.Contains(runtime.Version(), "1.4") {
		return
	}

	withTestProcess("cgotest", t, func(p *Process, fixture protest.Fixture) {
		pc, err := p.FindFunctionLocation("main.main", true, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, err = p.SetBreakpoint(pc)
		if err != nil {
			t.Fatal(err)
		}
		err = p.Continue()
		if err != nil {
			t.Fatal(err)
		}
		err = p.Next()
		if err != nil {
			t.Fatal(err)
		}
	})
}

type loc struct {
	line int
	fn   string
}

func (l1 *loc) match(l2 Stackframe) bool {
	if l1.line >= 0 {
		if l1.line != l2.Call.Line {
			return false
		}
	}
	return l1.fn == l2.Call.Fn.Name
}

func TestStacktrace(t *testing.T) {
	stacks := [][]loc{
		[]loc{{4, "main.stacktraceme"}, {8, "main.func1"}, {16, "main.main"}},
		[]loc{{4, "main.stacktraceme"}, {8, "main.func1"}, {12, "main.func2"}, {17, "main.main"}},
	}
	withTestProcess("stacktraceprog", t, func(p *Process, fixture protest.Fixture) {
		bp, err := setFunctionBreakpoint(p, "main.stacktraceme")
		assertNoError(err, t, "BreakByLocation()")

		for i := range stacks {
			assertNoError(p.Continue(), t, "Continue()")
			locations, err := p.CurrentThread.Stacktrace(40)
			assertNoError(err, t, "Stacktrace()")

			if len(locations) != len(stacks[i])+2 {
				t.Fatalf("Wrong stack trace size %d %d\n", len(locations), len(stacks[i])+2)
			}

			t.Logf("Stacktrace %d:\n", i)
			for i := range locations {
				t.Logf("\t%s:%d\n", locations[i].Call.File, locations[i].Call.Line)
			}

			for j := range stacks[i] {
				if !stacks[i][j].match(locations[j]) {
					t.Fatalf("Wrong stack trace pos %d\n", j)
				}
			}
		}

		p.ClearBreakpoint(bp.Addr)
		p.Continue()
	})
}

func TestStacktrace2(t *testing.T) {
	withTestProcess("retstack", t, func(p *Process, fixture protest.Fixture) {
		assertNoError(p.Continue(), t, "Continue()")

		locations, err := p.CurrentThread.Stacktrace(40)
		assertNoError(err, t, "Stacktrace()")
		if !stackMatch([]loc{loc{-1, "main.f"}, loc{16, "main.main"}}, locations) {
			for i := range locations {
				t.Logf("\t%s:%d [%s]\n", locations[i].Call.File, locations[i].Call.Line, locations[i].Call.Fn.Name)
			}
			t.Fatalf("Stack error at main.f()\n", locations)
		}

		assertNoError(p.Continue(), t, "Continue()")
		locations, err = p.CurrentThread.Stacktrace(40)
		assertNoError(err, t, "Stacktrace()")
		if !stackMatch([]loc{loc{-1, "main.g"}, loc{17, "main.main"}}, locations) {
			for i := range locations {
				t.Logf("\t%s:%d [%s]\n", locations[i].Call.File, locations[i].Call.Line, locations[i].Call.Fn.Name)
			}
			t.Fatalf("Stack error at main.g()\n", locations)
		}
	})

}

func stackMatch(stack []loc, locations []Stackframe) bool {
	if len(stack) > len(locations) {
		return false
	}
	for i := range stack {
		if !stack[i].match(locations[i]) {
			return false
		}
	}
	return true
}

func TestStacktraceGoroutine(t *testing.T) {
	mainStack := []loc{{12, "main.stacktraceme"}, {21, "main.main"}}
	agoroutineStack := []loc{{-1, "runtime.gopark"}, {-1, "runtime.goparkunlock"}, {-1, "runtime.chansend"}, {-1, "runtime.chansend1"}, {8, "main.agoroutine"}}

	withTestProcess("goroutinestackprog", t, func(p *Process, fixture protest.Fixture) {
		bp, err := setFunctionBreakpoint(p, "main.stacktraceme")
		assertNoError(err, t, "BreakByLocation()")

		assertNoError(p.Continue(), t, "Continue()")

		gs, err := p.GoroutinesInfo()
		assertNoError(err, t, "GoroutinesInfo")

		agoroutineCount := 0
		mainCount := 0

		for i, g := range gs {
			locations, err := p.GoroutineStacktrace(g, 40)
			assertNoError(err, t, "GoroutineStacktrace()")

			if stackMatch(mainStack, locations) {
				mainCount++
			}

			if stackMatch(agoroutineStack, locations) {
				agoroutineCount++
			} else {
				t.Logf("Non-goroutine stack: %d (%d)", i, len(locations))
				for i := range locations {
					name := ""
					if locations[i].Call.Fn != nil {
						name = locations[i].Call.Fn.Name
					}
					t.Logf("\t%s:%d %s\n", locations[i].Call.File, locations[i].Call.Line, name)
				}
			}
		}

		if mainCount != 1 {
			t.Fatalf("Main goroutine stack not found")
		}

		if agoroutineCount != 10 {
			t.Fatalf("Goroutine stacks not found (%d)", agoroutineCount)
		}

		p.ClearBreakpoint(bp.Addr)
		p.Continue()
	})
}

func TestKill(t *testing.T) {
	withTestProcess("testprog", t, func(p *Process, fixture protest.Fixture) {
		if err := p.Kill(); err != nil {
			t.Fatal(err)
		}
		if p.Exited() != true {
			t.Fatal("expected process to have exited")
		}
		if runtime.GOOS == "linux" {
			_, err := os.Open(fmt.Sprintf("/proc/%d/", p.Pid))
			if err == nil {
				t.Fatal("process has not exited", p.Pid)
			}
		}
	})
}

func testGSupportFunc(name string, t *testing.T, p *Process, fixture protest.Fixture) {
	bp, err := setFunctionBreakpoint(p, "main.main")
	assertNoError(err, t, name+": BreakByLocation()")

	assertNoError(p.Continue(), t, name+": Continue()")

	g, err := p.CurrentThread.GetG()
	assertNoError(err, t, name+": GetG()")

	if g == nil {
		t.Fatal(name + ": g was nil")
	}

	t.Logf(name+": g is: %v", g)

	p.ClearBreakpoint(bp.Addr)
}

func TestGetG(t *testing.T) {
	withTestProcess("testprog", t, func(p *Process, fixture protest.Fixture) {
		testGSupportFunc("nocgo", t, p, fixture)
	})

	// On OSX with Go < 1.5 CGO is not supported due to: https://github.com/golang/go/issues/8973
	if runtime.GOOS == "darwin" && strings.Contains(runtime.Version(), "1.4") {
		return
	}

	withTestProcess("cgotest", t, func(p *Process, fixture protest.Fixture) {
		testGSupportFunc("cgo", t, p, fixture)
	})
}

func TestContinueMulti(t *testing.T) {
	withTestProcess("integrationprog", t, func(p *Process, fixture protest.Fixture) {
		bp1, err := setFunctionBreakpoint(p, "main.main")
		assertNoError(err, t, "BreakByLocation()")

		bp2, err := setFunctionBreakpoint(p, "main.sayhi")
		assertNoError(err, t, "BreakByLocation()")

		mainCount := 0
		sayhiCount := 0
		for {
			err := p.Continue()
			if p.Exited() {
				break
			}
			assertNoError(err, t, "Continue()")

			if p.CurrentBreakpoint().ID == bp1.ID {
				mainCount++
			}

			if p.CurrentBreakpoint().ID == bp2.ID {
				sayhiCount++
			}
		}

		if mainCount != 1 {
			t.Fatalf("Main breakpoint hit wrong number of times: %d\n", mainCount)
		}

		if sayhiCount != 3 {
			t.Fatalf("Sayhi breakpoint hit wrong number of times: %d\n", sayhiCount)
		}
	})
}

func versionAfterOrEqual(t *testing.T, verStr string, ver GoVersion) {
	pver, ok := parseVersionString(verStr)
	if !ok {
		t.Fatalf("Could not parse version string <%s>", verStr)
	}
	if !pver.AfterOrEqual(ver) {
		t.Fatalf("Version <%s> parsed as %v not after %v", verStr, pver, ver)
	}
	t.Logf("version string <%s> → %v", verStr, ver)
}

func TestParseVersionString(t *testing.T) {
	versionAfterOrEqual(t, "go1.4", GoVersion{1, 4, 0, 0, 0})
	versionAfterOrEqual(t, "go1.5.0", GoVersion{1, 5, 0, 0, 0})
	versionAfterOrEqual(t, "go1.4.2", GoVersion{1, 4, 2, 0, 0})
	versionAfterOrEqual(t, "go1.5beta2", GoVersion{1, 5, -1, 2, 0})
	versionAfterOrEqual(t, "go1.5rc2", GoVersion{1, 5, -1, 0, 2})
	ver, ok := parseVersionString("devel +17efbfc Tue Jul 28 17:39:19 2015 +0000 linux/amd64")
	if !ok {
		t.Fatalf("Could not parse devel version string")
	}
	if !ver.IsDevel() {
		t.Fatalf("Devel version string not correctly recognized")
	}
}

func TestBreakpointOnFunctionEntry(t *testing.T) {
	withTestProcess("testprog", t, func(p *Process, fixture protest.Fixture) {
		addr, err := p.FindFunctionLocation("main.main", false, 0)
		assertNoError(err, t, "FindFunctionLocation()")
		_, err = p.SetBreakpoint(addr)
		assertNoError(err, t, "SetBreakpoint()")
		assertNoError(p.Continue(), t, "Continue()")
		_, ln := currentLineNumber(p, t)
		if ln != 17 {
			t.Fatalf("Wrong line number: %d (expected: 17)\n", ln)
		}
	})
}

func TestProcessReceivesSIGCHLD(t *testing.T) {
	withTestProcess("sigchldprog", t, func(p *Process, fixture protest.Fixture) {
		err := p.Continue()
		_, ok := err.(ProcessExitedError)
		if !ok {
			t.Fatalf("Continue() returned unexpected error type %s", err)
		}
	})
}

func TestIssue239(t *testing.T) {
	withTestProcess("is sue239", t, func(p *Process, fixture protest.Fixture) {
		pos, _, err := p.goSymTable.LineToPC(fixture.Source, 17)
		assertNoError(err, t, "LineToPC()")
		_, err = p.SetBreakpoint(pos)
		assertNoError(err, t, fmt.Sprintf("SetBreakpoint(%d)", pos))
		assertNoError(p.Continue(), t, fmt.Sprintf("Continue()"))
	})
}
