package proc

import (
	"bytes"
	"debug/dwarf"
	"errors"
	"fmt"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/printer"
	"go/token"
	"reflect"
	"strconv"

	"github.com/go-delve/delve/pkg/astutil"
	"github.com/go-delve/delve/pkg/dwarf/godwarf"
	"github.com/go-delve/delve/pkg/dwarf/op"
	"github.com/go-delve/delve/pkg/dwarf/reader"
	"github.com/go-delve/delve/pkg/goversion"
	"github.com/go-delve/delve/pkg/proc/evalop"
	"github.com/go-delve/delve/pkg/proc/internal/ebpf"
)

const (
	// UnrecoveredPanic is the name given to the unrecovered panic breakpoint.
	UnrecoveredPanic = "unrecovered-panic"

	// FatalThrow is the name given to the breakpoint triggered when the target
	// process dies because of a fatal runtime error.
	FatalThrow = "runtime-fatal-throw"

	// HardcodedBreakpoint is the name given to hardcoded breakpoints (for
	// example: calls to runtime.Breakpoint)
	HardcodedBreakpoint = "hardcoded-breakpoint"

	unrecoveredPanicID    = -1
	fatalThrowID          = -2
	hardcodedBreakpointID = -3

	NoLogicalID = -1000 // Logical breakpoint ID for breakpoints internal breakpoints.
)

// Breakpoint represents a physical breakpoint. Stores information on the break
// point including the byte of data that originally was stored at that
// address.
type Breakpoint struct {
	// File & line information for printing.
	FunctionName string
	File         string
	Line         int

	Addr         uint64 // Address breakpoint is set for.
	OriginalData []byte // If software breakpoint, the data we replace with breakpoint instruction.

	WatchExpr     string
	WatchType     WatchType
	HWBreakIndex  uint8 // hardware breakpoint index
	watchStackOff int64 // for watchpoints of stack variables, offset of the address from top of the stack

	// Breaklets is the list of overlapping breakpoints on this physical breakpoint.
	// There can be at most one UserBreakpoint in this list but multiple internal breakpoints are allowed.
	Breaklets []*Breaklet

	// Breakpoint information
	Logical *LogicalBreakpoint

	// ReturnInfo describes how to collect return variables when this
	// breakpoint is hit as a return breakpoint.
	returnInfo *returnBreakpointInfo

	// RootFuncName is the name of the root function from where tracing needs to be done
	RootFuncName string
	// TraceFollowCalls indicates the depth of tracing
	TraceFollowCalls int
}

// Breaklet represents one of multiple breakpoints that can overlap on a
// single physical breakpoint.
type Breaklet struct {
	// Kind describes whether this is a stepping breakpoint (for next'ing or
	// stepping).
	Kind BreakpointKind

	LogicalID int // ID of the logical breakpoint that owns this physical breakpoint

	// Cond: if not nil the breakpoint will be triggered only if evaluating Cond returns true
	Cond ast.Expr

	// DeferReturns: when kind == NextDeferBreakpoint this breakpoint
	// will also check if the caller is runtime.gopanic or if the return
	// address is in the DeferReturns array.
	// Next uses NextDeferBreakpoints for the breakpoint it sets on the
	// deferred function, DeferReturns is populated with the
	// addresses of calls to runtime.deferreturn in the current
	// function. This ensures that the breakpoint on the deferred
	// function only triggers on panic or on the defer call to
	// the function, not when the function is called directly
	DeferReturns []uint64

	// checkPanicCall checks that the breakpoint happened while the function was
	// called by a panic. It is only checked for WatchOutOfScopeBreakpoint Kind.
	checkPanicCall bool

	// callback is called if every other condition for this breaklet is met,
	// the return value will determine if the breaklet should be considered
	// active.
	// The callback can have side-effects.
	callback func(th Thread, p *Target) (bool, error)

	// For WatchOutOfScopeBreakpoints and StackResizeBreakpoints the watchpoint
	// field contains the watchpoint related to this out of scope sentinel.
	watchpoint *Breakpoint
}

// BreakpointKind determines the behavior of delve when the
// breakpoint is reached.
type BreakpointKind uint16

const (
	// UserBreakpoint is a user set breakpoint
	UserBreakpoint BreakpointKind = (1 << iota)
	// NextBreakpoint is a breakpoint set by Next, Continue
	// will stop on it and delete it
	NextBreakpoint
	// NextDeferBreakpoint is a breakpoint set by Next on the
	// first deferred function. In addition to checking their condition
	// breakpoints of this kind will also check that the function has been
	// called by runtime.gopanic or through runtime.deferreturn.
	NextDeferBreakpoint
	// StepBreakpoint is a breakpoint set by Step on a CALL instruction,
	// Continue will set a new breakpoint (of NextBreakpoint kind) on the
	// destination of CALL, delete this breakpoint and then continue again
	StepBreakpoint

	// WatchOutOfScopeBreakpoint is a breakpoint used to detect when a watched
	// stack variable goes out of scope.
	WatchOutOfScopeBreakpoint

	// StackResizeBreakpoint is a breakpoint used to detect stack resizes to
	// adjust the watchpoint of stack variables.
	StackResizeBreakpoint

	// PluginOpenBreakpoint is a breakpoint used to detect that a plugin has
	// been loaded and we should try to enable suspended breakpoints.
	PluginOpenBreakpoint

	// StepIntoNewProc is a breakpoint used to step into a newly created
	// goroutine.
	StepIntoNewProcBreakpoint

	// NextInactivatedBreakpoint a NextBreakpoint that has been inactivated, see rangeFrameInactivateNextBreakpoints
	NextInactivatedBreakpoint

	StepIntoRangeOverFuncBodyBreakpoint

	steppingMask = NextBreakpoint | NextDeferBreakpoint | StepBreakpoint | StepIntoNewProcBreakpoint | NextInactivatedBreakpoint | StepIntoRangeOverFuncBodyBreakpoint
)

// WatchType is the watchpoint type
type WatchType uint8

const (
	WatchRead WatchType = 1 << iota
	WatchWrite
)

// Read returns true if the hardware breakpoint should trigger on memory reads.
func (wtype WatchType) Read() bool {
	return wtype&WatchRead != 0
}

// Write returns true if the hardware breakpoint should trigger on memory writes.
func (wtype WatchType) Write() bool {
	return wtype&WatchWrite != 0
}

// Size returns the size in bytes of the hardware breakpoint.
func (wtype WatchType) Size() int {
	return int(wtype >> 4)
}

// withSize returns a new HWBreakType with the size set to the specified value
func (wtype WatchType) withSize(sz uint8) WatchType {
	return WatchType((sz << 4) | uint8(wtype&0xf))
}

var ErrHWBreakUnsupported = errors.New("hardware breakpoints not implemented")

func (bp *Breakpoint) String() string {
	return fmt.Sprintf("Breakpoint %d at %#v %s:%d", bp.LogicalID(), bp.Addr, bp.File, bp.Line)
}

func (bp *Breakpoint) LogicalID() int {
	for _, breaklet := range bp.Breaklets {
		if breaklet.Kind == UserBreakpoint {
			return breaklet.LogicalID
		}
	}
	return NoLogicalID
}

// VerboseDescr returns a string describing parts of the breakpoint struct
// that aren't otherwise user visible, for debugging purposes.
func (bp *Breakpoint) VerboseDescr() []string {
	r := []string{}

	r = append(r, fmt.Sprintf("OriginalData=%#x", bp.OriginalData))

	if bp.WatchType != 0 {
		r = append(r, fmt.Sprintf("HWBreakIndex=%#x watchStackOff=%#x", bp.HWBreakIndex, bp.watchStackOff))
	}

	lbp := bp.Logical

	for _, breaklet := range bp.Breaklets {
		switch breaklet.Kind {
		case UserBreakpoint:
			r = append(r, fmt.Sprintf("User Cond=%q HitCond=%v", astutil.ExprToString(breaklet.Cond), lbp.HitCond()))
		case NextBreakpoint:
			r = append(r, fmt.Sprintf("Next Cond=%q", astutil.ExprToString(breaklet.Cond)))
		case NextDeferBreakpoint:
			r = append(r, fmt.Sprintf("NextDefer Cond=%q DeferReturns=%#x", astutil.ExprToString(breaklet.Cond), breaklet.DeferReturns))
		case StepBreakpoint:
			r = append(r, fmt.Sprintf("Step Cond=%q", astutil.ExprToString(breaklet.Cond)))
		case WatchOutOfScopeBreakpoint:
			r = append(r, fmt.Sprintf("WatchOutOfScope Cond=%q checkPanicCall=%v", astutil.ExprToString(breaklet.Cond), breaklet.checkPanicCall))
		case StackResizeBreakpoint:
			r = append(r, fmt.Sprintf("StackResizeBreakpoint Cond=%q", astutil.ExprToString(breaklet.Cond)))
		case PluginOpenBreakpoint:
			r = append(r, "PluginOpenBreakpoint")
		case StepIntoNewProcBreakpoint:
			r = append(r, "StepIntoNewProcBreakpoint")
		case NextInactivatedBreakpoint:
			r = append(r, "NextInactivatedBreakpoint")
		case StepIntoRangeOverFuncBodyBreakpoint:
			r = append(r, "StepIntoRangeOverFuncBodyBreakpoint Cond=%q", astutil.ExprToString(breaklet.Cond))
		default:
			r = append(r, fmt.Sprintf("Unknown %d", breaklet.Kind))
		}
	}
	return r
}

// BreakpointExistsError is returned when trying to set a breakpoint at
// an address that already has a breakpoint set for it.
type BreakpointExistsError struct {
	File string
	Line int
	Addr uint64
}

func (bpe BreakpointExistsError) Error() string {
	return fmt.Sprintf("Breakpoint exists at %s:%d at %x", bpe.File, bpe.Line, bpe.Addr)
}

// InvalidAddressError represents the result of
// attempting to set a breakpoint at an invalid address.
type InvalidAddressError struct {
	Address uint64
}

func (iae InvalidAddressError) Error() string {
	return fmt.Sprintf("Invalid address %#v\n", iae.Address)
}

type returnBreakpointInfo struct {
	retFrameCond ast.Expr
	fn           *Function
	frameOffset  int64
	spOffset     int64
}

// CheckCondition evaluates bp's condition on thread.
func (bp *Breakpoint) checkCondition(tgt *Target, thread Thread, bpstate *BreakpointState) {
	*bpstate = BreakpointState{Breakpoint: bp, Active: false, Stepping: false, SteppingInto: false, CondError: nil}
	for _, breaklet := range bp.Breaklets {
		bpstate.checkCond(tgt, breaklet, thread)
	}
}

func (bpstate *BreakpointState) checkCond(tgt *Target, breaklet *Breaklet, thread Thread) {
	var condErr error
	active := true
	if breaklet.Cond != nil {
		active, condErr = evalBreakpointCondition(tgt, thread, breaklet.Cond)
	}

	if condErr != nil && bpstate.CondError == nil {
		bpstate.CondError = condErr
	}
	if !active {
		return
	}

	switch breaklet.Kind {
	case UserBreakpoint:
		var goroutineID int64
		lbp := bpstate.Breakpoint.Logical
		if lbp != nil {
			if g, err := GetG(thread); err == nil {
				goroutineID = g.ID
				lbp.HitCount[goroutineID]++
			}
			lbp.TotalHitCount++
		}
		active = checkHitCond(lbp, goroutineID)

	case StepBreakpoint, NextBreakpoint, NextDeferBreakpoint:
		nextDeferOk := true
		if breaklet.Kind&NextDeferBreakpoint != 0 {
			frames, err := ThreadStacktrace(tgt, thread, 2)
			if err == nil {
				nextDeferOk, _ = isPanicCall(frames)
				if !nextDeferOk {
					nextDeferOk, _ = isDeferReturnCall(frames, breaklet.DeferReturns)
				}
			}
		}
		active = active && nextDeferOk

	case WatchOutOfScopeBreakpoint:
		if breaklet.checkPanicCall {
			frames, err := ThreadStacktrace(tgt, thread, 2)
			if err == nil {
				ipc, _ := isPanicCall(frames)
				active = active && ipc
			}
		}

	case StackResizeBreakpoint, PluginOpenBreakpoint, StepIntoNewProcBreakpoint, StepIntoRangeOverFuncBodyBreakpoint:
		// no further checks

	case NextInactivatedBreakpoint:
		active = false

	default:
		bpstate.CondError = fmt.Errorf("internal error unknown breakpoint kind %v", breaklet.Kind)
	}

	if active {
		if breaklet.callback != nil {
			var err error
			active, err = breaklet.callback(thread, tgt)
			if err != nil && bpstate.CondError == nil {
				bpstate.CondError = err
			}
		}
		bpstate.Active = active
	}

	if bpstate.Active {
		switch breaklet.Kind {
		case NextBreakpoint, NextDeferBreakpoint:
			bpstate.Stepping = true
		case StepBreakpoint:
			bpstate.Stepping = true
			bpstate.SteppingInto = true
		case StepIntoRangeOverFuncBodyBreakpoint:
			bpstate.Stepping = true
			bpstate.SteppingIntoRangeOverFuncBody = true
		}
	}
}

// checkHitCond evaluates bp's hit condition on thread.
func checkHitCond(lbp *LogicalBreakpoint, goroutineID int64) bool {
	if lbp == nil || lbp.hitCond == nil {
		return true
	}
	hitCount := int(lbp.TotalHitCount)
	if lbp.HitCondPerG && goroutineID > 0 {
		hitCount = int(lbp.HitCount[goroutineID])
	}
	// Evaluate the breakpoint condition.
	switch lbp.hitCond.Op {
	case token.EQL:
		return hitCount == lbp.hitCond.Val
	case token.NEQ:
		return hitCount != lbp.hitCond.Val
	case token.GTR:
		return hitCount > lbp.hitCond.Val
	case token.LSS:
		return hitCount < lbp.hitCond.Val
	case token.GEQ:
		return hitCount >= lbp.hitCond.Val
	case token.LEQ:
		return hitCount <= lbp.hitCond.Val
	case token.REM:
		return hitCount%lbp.hitCond.Val == 0
	}
	return false
}

func isPanicCall(frames []Stackframe) (bool, int) {
	// In Go prior to 1.17 the call stack for a panic is:
	//  0. deferred function call
	//  1. runtime.callN
	//  2. runtime.gopanic
	// in Go after 1.17 it is either:
	//  0. deferred function call
	//  1. deferred call wrapper
	//  2. runtime.gopanic
	// or:
	//  0. deferred function call
	//  1. runtime.gopanic
	if len(frames) >= 3 && frames[2].Current.Fn != nil && frames[2].Current.Fn.Name == "runtime.gopanic" {
		return true, 2
	}
	if len(frames) >= 2 && frames[1].Current.Fn != nil && frames[1].Current.Fn.Name == "runtime.gopanic" {
		return true, 1
	}
	return false, 0
}

func isDeferReturnCall(frames []Stackframe, deferReturns []uint64) (bool, uint64) {
	if len(frames) >= 2 && (len(deferReturns) > 0) {
		// On Go 1.18 and later runtime.deferreturn doesn't use jmpdefer anymore,
		// it's a normal function making normal calls to deferred functions.
		if frames[1].Current.Fn != nil && frames[1].Current.Fn.Name == "runtime.deferreturn" {
			return true, 0
		}
	}
	if len(frames) >= 1 {
		for _, pc := range deferReturns {
			if frames[0].Ret == pc {
				return true, pc
			}
		}
	}
	return false, 0
}

// IsStepping returns true if bp is an stepping breakpoint.
// User-set breakpoints can overlap with stepping breakpoints, in that case
// both IsUser and IsStepping will be true.
func (bp *Breakpoint) IsStepping() bool {
	for _, breaklet := range bp.Breaklets {
		if breaklet.Kind&steppingMask != 0 {
			return true
		}
	}
	return false
}

// IsUser returns true if bp is a user-set breakpoint.
// User-set breakpoints can overlap with stepping breakpoints, in that case
// both IsUser and IsStepping will be true.
func (bp *Breakpoint) IsUser() bool {
	for _, breaklet := range bp.Breaklets {
		if breaklet.Kind == UserBreakpoint {
			return true
		}
	}
	return false
}

// UserBreaklet returns the user breaklet for this breakpoint, or nil if
// none exist.
func (bp *Breakpoint) UserBreaklet() *Breaklet {
	for _, breaklet := range bp.Breaklets {
		if breaklet.Kind == UserBreakpoint {
			return breaklet
		}
	}
	return nil
}

func evalBreakpointCondition(tgt *Target, thread Thread, cond ast.Expr) (bool, error) {
	if cond == nil {
		return true, nil
	}
	scope, err := GoroutineScope(tgt, thread)
	if err != nil {
		scope, err = ThreadScope(tgt, thread)
		if err != nil {
			return true, err
		}
	}
	v, err := scope.evalAST(cond)
	if err != nil {
		return true, fmt.Errorf("error evaluating expression: %v", err)
	}
	if v.Kind != reflect.Bool {
		return true, errors.New("condition expression not boolean")
	}
	v.loadValue(loadFullValue)
	if v.Unreadable != nil {
		return true, fmt.Errorf("condition expression unreadable: %v", v.Unreadable)
	}
	return constant.BoolVal(v.Value), nil
}

// NoBreakpointError is returned when trying to
// clear a breakpoint that does not exist.
type NoBreakpointError struct {
	Addr uint64
}

func (nbp NoBreakpointError) Error() string {
	return fmt.Sprintf("no breakpoint at %#v", nbp.Addr)
}

// BreakpointMap represents an (address, breakpoint) map.
type BreakpointMap struct {
	M map[uint64]*Breakpoint

	// Logical is a map of logical breakpoints.
	Logical map[int]*LogicalBreakpoint

	// WatchOutOfScope is the list of watchpoints that went out of scope during
	// the last resume operation
	WatchOutOfScope []*Breakpoint
}

// NewBreakpointMap creates a new BreakpointMap.
func NewBreakpointMap() BreakpointMap {
	return BreakpointMap{
		M: make(map[uint64]*Breakpoint),
	}
}

// SetBreakpoint sets a breakpoint at addr, and stores it in the process wide
// break point table.
func (t *Target) SetBreakpoint(logicalID int, addr uint64, kind BreakpointKind, cond ast.Expr) (*Breakpoint, error) {
	return t.setBreakpointInternal(logicalID, addr, kind, 0, cond)
}

// SetEBPFTracepoint will attach a uprobe to the function
// specified by 'fnName'.
func (t *Target) SetEBPFTracepoint(fnName string) error {
	// Not every OS/arch that we support has support for eBPF,
	// so check early and return an error if this is called on an
	// unsupported system.
	if !t.proc.SupportsBPF() {
		return errors.New("eBPF is not supported")
	}
	fns, err := t.BinInfo().FindFunction(fnName)
	if err != nil {
		return err
	}

	// Get information on the Goroutine so we can tell the
	// eBPF program where to find it in order to get the
	// goroutine ID.
	rdr := t.BinInfo().Images[0].DwarfReader()
	rdr.SeekToTypeNamed("runtime.g")
	typ, err := t.BinInfo().findType("runtime.g")
	if err != nil {
		return errors.New("could not find type for runtime.g")
	}
	var goidOffset int64
	switch t := typ.(type) {
	case *godwarf.StructType:
		for _, field := range t.Field {
			if field.Name == "goid" {
				goidOffset = field.ByteOffset
				break
			}
		}
	}

	for _, fn := range fns {
		err := t.setEBPFTracepointOnFunc(fn, goidOffset)
		if err != nil {
			return err
		}
	}
	return nil
}

func (t *Target) setEBPFTracepointOnFunc(fn *Function, goidOffset int64) error {
	// Start putting together the argument map. This will tell the eBPF program
	// all of the arguments we want to trace and how to find them.

	// Start looping through each argument / return parameter for the function we
	// are setting the uprobe on. Parse location information so that we can pass it
	// along to the eBPF program.
	dwarfTree, err := fn.cu.image.getDwarfTree(fn.offset)
	if err != nil {
		return err
	}
	variablesFlags := reader.VariablesOnlyVisible
	if t.BinInfo().Producer() != "" && goversion.ProducerAfterOrEqual(t.BinInfo().Producer(), 1, 15) {
		variablesFlags |= reader.VariablesTrustDeclLine
	}
	_, l := t.BinInfo().EntryLineForFunc(fn)

	var args []ebpf.UProbeArgMap
	varEntries := reader.Variables(dwarfTree, fn.Entry, l, variablesFlags)
	for _, entry := range varEntries {
		_, dt, err := readVarEntry(entry.Tree, fn.cu.image)
		if err != nil {
			return err
		}

		offset, pieces, _, err := t.BinInfo().Location(entry, dwarf.AttrLocation, fn.Entry, op.DwarfRegisters{}, nil)
		if err != nil {
			return err
		}
		paramPieces := make([]int, 0, len(pieces))
		for _, piece := range pieces {
			if piece.Kind == op.RegPiece {
				paramPieces = append(paramPieces, int(piece.Val))
			}
		}
		isret, _ := entry.Val(dwarf.AttrVarParam).(bool)
		offset += int64(t.BinInfo().Arch.PtrSize())
		args = append(args, ebpf.UProbeArgMap{
			Offset: offset,
			Size:   dt.Size(),
			Kind:   dt.Common().ReflectKind,
			Pieces: paramPieces,
			InReg:  len(pieces) > 0,
			Ret:    isret,
		})
	}

	//TODO(aarzilli): inlined calls?

	// Finally, set the uprobe on the function.
	return t.proc.SetUProbe(fn.Name, goidOffset, args)
}

// SetWatchpoint sets a data breakpoint at addr and stores it in the
// process wide break point table.
func (t *Target) SetWatchpoint(logicalID int, scope *EvalScope, expr string, wtype WatchType, cond ast.Expr) (*Breakpoint, error) {
	if (wtype&WatchWrite == 0) && (wtype&WatchRead == 0) {
		return nil, errors.New("at least one of read and write must be set for watchpoint")
	}

	n, err := parser.ParseExpr(expr)
	if err != nil {
		return nil, err
	}
	xv, err := scope.evalAST(n)
	if err != nil {
		return nil, err
	}
	if xv.Addr == 0 || xv.Flags&VariableFakeAddress != 0 || xv.DwarfType == nil {
		return nil, fmt.Errorf("can not watch %q", expr)
	}
	if xv.Unreadable != nil {
		return nil, fmt.Errorf("expression %q is unreadable: %v", expr, xv.Unreadable)
	}
	if xv.Kind == reflect.UnsafePointer || xv.Kind == reflect.Invalid {
		return nil, fmt.Errorf("can not watch variable of type %s", xv.Kind.String())
	}

	// Special handling for interface types
	if xv.Kind == reflect.Interface {
		// For interfaces, we want to watch the data they point to
		// Read the interface to get the data pointer
		_, data, _ := xv.readInterface()
		if xv.Unreadable != nil {
			return nil, fmt.Errorf("error reading interface %q: %v", expr, xv.Unreadable)
		}
		if data == nil {
			return nil, fmt.Errorf("invalid interface %q", expr)
		}

		// Use the data field as our watch target
		xv = data
		expr = expr + " (interface data)"
	}

	sz := xv.DwarfType.Size()
	if sz <= 0 || sz > int64(t.BinInfo().Arch.PtrSize()) {
		//TODO(aarzilli): it is reasonable to expect to be able to watch string
		//variables and we could support it by watching certain member fields here.
		return nil, fmt.Errorf("can not watch variable of type %s", xv.DwarfType.String())
	}

	stackWatch := scope.g != nil && !scope.g.SystemStack && xv.Addr >= scope.g.stack.lo && xv.Addr < scope.g.stack.hi

	if stackWatch && wtype&WatchRead != 0 {
		// In theory this would work except for the fact that the runtime will
		// read them randomly to resize stacks so it doesn't make sense to do
		// this.
		return nil, errors.New("can not watch stack allocated variable for reads")
	}

	bp, err := t.setBreakpointInternal(logicalID, xv.Addr, UserBreakpoint, wtype.withSize(uint8(sz)), cond)
	if err != nil {
		return bp, err
	}
	bp.WatchExpr = expr

	if stackWatch {
		bp.watchStackOff = int64(bp.Addr) - int64(scope.g.stack.hi)
		err := t.setStackWatchBreakpoints(scope, bp)
		if err != nil {
			return bp, err
		}
	}

	return bp, nil
}

func (t *Target) setBreakpointInternal(logicalID int, addr uint64, kind BreakpointKind, wtype WatchType, cond ast.Expr) (*Breakpoint, error) {
	if valid, err := t.Valid(); !valid {
		recorded, _ := t.recman.Recorded()
		if !recorded {
			return nil, err
		}
	}
	bpmap := t.Breakpoints()
	newBreaklet := &Breaklet{Kind: kind, Cond: cond}
	if kind == UserBreakpoint {
		newBreaklet.LogicalID = logicalID
	}

	setLogicalBreakpoint := func(bp *Breakpoint) {
		if kind != UserBreakpoint || bp.Logical != nil {
			return
		}
		if bpmap.Logical == nil {
			bpmap.Logical = make(map[int]*LogicalBreakpoint)
		}
		lbp := bpmap.Logical[logicalID]
		if lbp == nil {
			lbp = &LogicalBreakpoint{LogicalID: logicalID}
			lbp.HitCount = make(map[int64]uint64)
			lbp.enabled = true
			lbp.condSatisfiable = true
			bpmap.Logical[logicalID] = lbp
		}
		bp.Logical = lbp
		breaklet := bp.UserBreaklet()
		if breaklet != nil && breaklet.Cond == nil {
			breaklet.Cond = lbp.cond
		}
		if lbp.File == "" && lbp.Line == 0 {
			lbp.File = bp.File
			lbp.Line = bp.Line
		} else if bp.File != lbp.File || bp.Line != lbp.Line {
			lbp.File = "<multiple locations>"
			lbp.Line = 0
		}
		fn := t.BinInfo().PCToFunc(bp.Addr)
		if fn != nil {
			lbp.FunctionName = fn.NameWithoutTypeParams()
		}
	}

	if bp, ok := bpmap.M[addr]; ok {
		if !bp.canOverlap(kind) {
			return bp, BreakpointExistsError{bp.File, bp.Line, bp.Addr}
		}
		bp.Breaklets = append(bp.Breaklets, newBreaklet)
		setLogicalBreakpoint(bp)
		return bp, nil
	}

	f, l, fn := t.BinInfo().PCToLine(addr)

	fnName := ""
	if fn != nil {
		fnName = fn.Name
	}

	hwidx := uint8(0)
	if wtype != 0 {
		m := make(map[uint8]bool)
		for _, bp := range bpmap.M {
			if bp.WatchType != 0 {
				m[bp.HWBreakIndex] = true
			}
		}
		for hwidx = 0; true; hwidx++ {
			if !m[hwidx] {
				break
			}
		}
	}

	newBreakpoint := &Breakpoint{
		FunctionName: fnName,
		WatchType:    wtype,
		HWBreakIndex: hwidx,
		File:         f,
		Line:         l,
		Addr:         addr,
	}

	err := t.proc.WriteBreakpoint(newBreakpoint)
	if err != nil {
		return nil, err
	}

	newBreakpoint.Breaklets = append(newBreakpoint.Breaklets, newBreaklet)
	setLogicalBreakpoint(newBreakpoint)

	bpmap.M[addr] = newBreakpoint

	return newBreakpoint, nil
}

// canOverlap returns true if a breakpoint of kind can be overlapped to the
// already existing breaklets in bp.
// At most one user breakpoint can be set but multiple internal breakpoints are allowed.
// All other internal breakpoints are allowed to overlap freely.
func (bp *Breakpoint) canOverlap(kind BreakpointKind) bool {
	if kind == UserBreakpoint {
		return !bp.IsUser()
	}
	return true
}

// ClearBreakpoint clears the breakpoint at addr.
func (t *Target) ClearBreakpoint(addr uint64) error {
	if valid, err := t.Valid(); !valid {
		recorded, _ := t.recman.Recorded()
		if !recorded {
			return err
		}
	}
	bp, ok := t.Breakpoints().M[addr]
	if !ok {
		return NoBreakpointError{Addr: addr}
	}

	for i := range bp.Breaklets {
		if bp.Breaklets[i].Kind == UserBreakpoint {
			bp.Breaklets[i] = nil
			if bp.WatchExpr == "" {
				bp.Logical = nil
			}
		}
	}

	_, err := t.finishClearBreakpoint(bp)
	if err != nil {
		return err
	}

	if bp.WatchExpr != "" && bp.watchStackOff != 0 {
		// stack watchpoint, must remove all its WatchOutOfScopeBreakpoints/StackResizeBreakpoints
		err := t.clearStackWatchBreakpoints(bp)
		if err != nil {
			return err
		}
	}

	return nil
}

// ClearSteppingBreakpoints removes all stepping breakpoints from the map,
// calling clearBreakpoint on each one.
func (t *Target) ClearSteppingBreakpoints() error {
	bpmap := t.Breakpoints()
	threads := t.ThreadList()
	for _, bp := range bpmap.M {
		for i := range bp.Breaklets {
			if bp.Breaklets[i].Kind&steppingMask != 0 {
				bp.Breaklets[i] = nil
			}
		}
		cleared, err := t.finishClearBreakpoint(bp)
		if err != nil {
			return err
		}
		if cleared {
			for _, thread := range threads {
				if thread.Breakpoint().Breakpoint == bp {
					thread.Breakpoint().Clear()
				}
			}
		}
	}
	return nil
}

func (t *Target) clearInactivatedSteppingBreakpoint() error {
	threads := t.ThreadList()
	for _, bp := range t.Breakpoints().M {
		for i := range bp.Breaklets {
			if bp.Breaklets[i].Kind == NextInactivatedBreakpoint {
				bp.Breaklets[i] = nil
			}
		}
		cleared, err := t.finishClearBreakpoint(bp)
		if err != nil {
			return err
		}
		if cleared {
			for _, thread := range threads {
				if thread.Breakpoint().Breakpoint == bp {
					thread.Breakpoint().Clear()
				}
			}
		}
	}
	return nil
}

// finishClearBreakpoint clears nil breaklets from the breaklet list of bp
// and if it is empty erases the breakpoint.
// Returns true if the breakpoint was deleted
func (t *Target) finishClearBreakpoint(bp *Breakpoint) (bool, error) {
	oldBreaklets := bp.Breaklets
	bp.Breaklets = bp.Breaklets[:0]
	for _, breaklet := range oldBreaklets {
		if breaklet != nil {
			bp.Breaklets = append(bp.Breaklets, breaklet)
		}
	}
	if len(bp.Breaklets) > 0 {
		return false, nil
	}
	if err := t.proc.EraseBreakpoint(bp); err != nil {
		return false, err
	}

	delete(t.Breakpoints().M, bp.Addr)
	if bp.WatchExpr != "" && bp.Logical != nil {
		delete(t.Breakpoints().Logical, bp.Logical.LogicalID)
	}
	return true, nil
}

// HasSteppingBreakpoints returns true if bpmap has at least one stepping
// breakpoint set.
func (bpmap *BreakpointMap) HasSteppingBreakpoints() bool {
	for _, bp := range bpmap.M {
		if bp.IsStepping() {
			return true
		}
	}
	return false
}

// HasHWBreakpoints returns true if there are hardware breakpoints.
func (bpmap *BreakpointMap) HasHWBreakpoints() bool {
	for _, bp := range bpmap.M {
		if bp.WatchType != 0 {
			return true
		}
	}
	return false
}

func totalHitCountByName(lbpmap map[int]*LogicalBreakpoint, s string) (uint64, error) {
	for _, bp := range lbpmap {
		if bp.Name == s {
			return bp.TotalHitCount, nil
		}
	}
	return 0, fmt.Errorf("could not find breakpoint named %q", s)
}

func totalHitCountByID(lbpmap map[int]*LogicalBreakpoint, id int) (uint64, error) {
	for _, bp := range lbpmap {
		if bp.LogicalID == int(id) {
			return bp.TotalHitCount, nil
		}
	}
	return 0, fmt.Errorf("could not find breakpoint with ID = %d", id)
}

// BreakpointState describes the state of a breakpoint in a thread.
type BreakpointState struct {
	*Breakpoint
	// Active is true if the condition of any breaklet is met.
	Active bool
	// Stepping is true if one of the active breaklets is a stepping
	// breakpoint.
	Stepping bool
	// SteppingInto is true if one of the active stepping breaklets has Kind ==
	// StepBreakpoint.
	SteppingInto                  bool
	SteppingIntoRangeOverFuncBody bool
	// CondError contains any error encountered while evaluating the
	// breakpoint's condition.
	CondError error
}

// Clear zeros the struct.
func (bpstate *BreakpointState) Clear() {
	bpstate.Breakpoint = nil
	bpstate.Active = false
	bpstate.Stepping = false
	bpstate.SteppingInto = false
	bpstate.CondError = nil
}

func (bpstate *BreakpointState) String() string {
	s := bpstate.Breakpoint.String()
	if bpstate.Active {
		s += " active"
	}
	if bpstate.Stepping {
		s += " stepping"
	}
	return s
}

func configureReturnBreakpoint(bi *BinaryInfo, bp *Breakpoint, topframe *Stackframe, retFrameCond ast.Expr) {
	if topframe.Current.Fn == nil {
		return
	}
	bp.returnInfo = &returnBreakpointInfo{
		retFrameCond: retFrameCond,
		fn:           topframe.Current.Fn,
		frameOffset:  topframe.FrameOffset(),
		spOffset:     topframe.FrameOffset() - int64(bi.Arch.PtrSize()), // must be the value that SP had at the entry point of the function
	}
}

func (rbpi *returnBreakpointInfo) Collect(t *Target, thread Thread) []*Variable {
	if rbpi == nil {
		return nil
	}

	g, err := GetG(thread)
	if err != nil {
		return returnInfoError("could not get g", err, thread.BinInfo(), thread.ProcessMemory())
	}
	scope, err := GoroutineScope(t, thread)
	if err != nil {
		return returnInfoError("could not get scope", err, thread.BinInfo(), thread.ProcessMemory())
	}
	v, err := scope.evalAST(rbpi.retFrameCond)
	if err != nil || v.Unreadable != nil || v.Kind != reflect.Bool {
		// This condition was evaluated as part of the breakpoint condition
		// evaluation, if the errors happen they will be reported as part of the
		// condition errors.
		return nil
	}
	if !constant.BoolVal(v.Value) {
		// Breakpoint not hit as a return breakpoint.
		return nil
	}

	oldFrameOffset := rbpi.frameOffset + int64(g.stack.hi)
	oldSP := uint64(rbpi.spOffset + int64(g.stack.hi))
	err = fakeFunctionEntryScope(scope, rbpi.fn, oldFrameOffset, oldSP)
	if err != nil {
		return returnInfoError("could not read function entry", err, thread.BinInfo(), thread.ProcessMemory())
	}

	vars, err := scope.Locals(localsFakeFunctionEntryScope, "")
	if err != nil {
		return returnInfoError("could not evaluate return variables", err, thread.BinInfo(), thread.ProcessMemory())
	}
	vars = filterVariables(vars, func(v *Variable) bool {
		return (v.Flags & VariableReturnArgument) != 0
	})

	return vars
}

func returnInfoError(descr string, err error, bi *BinaryInfo, mem MemoryReadWriter) []*Variable {
	v := newConstant(constant.MakeString(fmt.Sprintf("%s: %v", descr, err.Error())), bi, mem)
	v.Name = "return value read error"
	return []*Variable{v}
}

// LogicalBreakpoint represents a breakpoint set by a user.
// A logical breakpoint can be associated with zero or many physical
// breakpoints.
// Where a physical breakpoint is associated with a specific instruction
// address a logical breakpoint is associated with a source code location.
// Therefore a logical breakpoint can be associated with zero or many
// physical breakpoints.
// It will have one or more physical breakpoints when source code has been
// inlined (or in the case of type parametric code).
// It will have zero physical breakpoints when it represents a deferred
// breakpoint for code that will be loaded in the future.
type LogicalBreakpoint struct {
	LogicalID    int
	Name         string
	FunctionName string
	File         string
	Line         int
	enabled      bool

	Set SetBreakpoint

	Tracepoint  bool // Tracepoint flag
	TraceReturn bool
	Goroutine   bool     // Retrieve goroutine information
	Stacktrace  int      // Number of stack frames to retrieve
	Variables   []string // Variables to evaluate
	LoadArgs    *LoadConfig
	LoadLocals  *LoadConfig

	HitCount      map[int64]uint64 // Number of times a breakpoint has been reached in a certain goroutine
	TotalHitCount uint64           // Number of times a breakpoint has been reached
	HitCondPerG   bool             // Use per goroutine hitcount as HitCond operand, instead of total hitcount

	// hitCond: if not nil the breakpoint will be triggered only if the evaluated HitCond returns
	// true with the TotalHitCount.
	hitCond *struct {
		Op  token.Token
		Val int
	}

	// cond: if not nil the breakpoint will be triggered only if evaluating Cond returns true
	cond ast.Expr

	// condSatisfiable is true when 'cond && hitCond' can potentially be true.
	condSatisfiable bool
	// condUsesHitCounts is true when 'cond' uses breakpoint hitcounts
	condUsesHitCounts bool

	UserData interface{} // Any additional information about the breakpoint
	// Name of root function from where tracing needs to be done
	RootFuncName string
	// depth of tracing
	TraceFollowCalls int
}

// SetBreakpoint describes how a breakpoint should be set.
type SetBreakpoint struct {
	FunctionName string
	File         string
	Line         int
	Expr         func(*Target) []uint64
	ExprString   string
	PidAddrs     []PidAddr
}

type PidAddr struct {
	Pid  int
	Addr uint64
}

// Enabled returns true if the breakpoint is enabled.
func (lbp *LogicalBreakpoint) Enabled() bool {
	return lbp.enabled
}

// HitCond returns the hit condition.
func (lbp *LogicalBreakpoint) HitCond() string {
	if lbp.hitCond == nil {
		return ""
	}
	return fmt.Sprintf("%s %d", lbp.hitCond.Op.String(), lbp.hitCond.Val)
}

func (lbp *LogicalBreakpoint) Cond() string {
	var buf bytes.Buffer
	printer.Fprint(&buf, token.NewFileSet(), lbp.cond)
	return buf.String()
}

func breakpointConditionSatisfiable(lbpmap map[int]*LogicalBreakpoint, lbp *LogicalBreakpoint) bool {
	if lbp.hitCond != nil && !lbp.HitCondPerG {
		switch lbp.hitCond.Op {
		case token.EQL, token.LEQ:
			if int(lbp.TotalHitCount) >= lbp.hitCond.Val {
				return false
			}
		case token.LSS:
			if int(lbp.TotalHitCount) >= lbp.hitCond.Val-1 {
				return false
			}
		}
	}
	if !lbp.condUsesHitCounts {
		return true
	}

	toint := func(x ast.Expr) (uint64, bool) {
		lit, ok := x.(*ast.BasicLit)
		if !ok || lit.Kind != token.INT {
			return 0, false
		}
		n, err := strconv.Atoi(lit.Value)
		return uint64(n), err == nil && n >= 0
	}

	hitcountexpr := func(x ast.Expr) (uint64, bool) {
		idx, ok := x.(*ast.IndexExpr)
		if !ok {
			return 0, false
		}
		selx, ok := idx.X.(*ast.SelectorExpr)
		if !ok {
			return 0, false
		}
		ident, ok := selx.X.(*ast.Ident)
		if !ok || ident.Name != evalop.BreakpointHitCountVarNamePackage || selx.Sel.Name != evalop.BreakpointHitCountVarName {
			return 0, false
		}
		lit, ok := idx.Index.(*ast.BasicLit)
		if !ok {
			return 0, false
		}
		switch lit.Kind {
		case token.INT:
			n, _ := strconv.Atoi(lit.Value)
			thc, err := totalHitCountByID(lbpmap, n)
			return thc, err == nil
		case token.STRING:
			v, _ := strconv.Unquote(lit.Value)
			thc, err := totalHitCountByName(lbpmap, v)
			return thc, err == nil
		default:
			return 0, false
		}
	}

	var satisf func(n ast.Node) bool
	satisf = func(n ast.Node) bool {
		parexpr, ok := n.(*ast.ParenExpr)
		if ok {
			return satisf(parexpr.X)
		}
		binexpr, ok := n.(*ast.BinaryExpr)
		if !ok {
			return true
		}
		switch binexpr.Op {
		case token.AND:
			return satisf(binexpr.X) && satisf(binexpr.Y)
		case token.OR:
			if !satisf(binexpr.X) {
				return false
			}
			if !satisf(binexpr.Y) {
				return false
			}
			return true
		case token.EQL, token.LEQ, token.LSS, token.NEQ, token.GTR, token.GEQ:
		default:
			return true
		}

		hitcount, ok1 := hitcountexpr(binexpr.X)
		val, ok2 := toint(binexpr.Y)
		if !ok1 || !ok2 {
			return true
		}

		switch binexpr.Op {
		case token.EQL:
			return hitcount == val
		case token.LEQ:
			return hitcount <= val
		case token.LSS:
			return hitcount < val
		case token.NEQ:
			return hitcount != val
		case token.GTR:
			return hitcount > val
		case token.GEQ:
			return hitcount >= val
		}
		return true
	}

	return satisf(lbp.cond)
}

func breakpointConditionUsesHitCounts(lbp *LogicalBreakpoint) bool {
	if lbp.cond == nil {
		return false
	}
	r := false
	ast.Inspect(lbp.cond, func(n ast.Node) bool {
		if r {
			return false
		}
		seln, ok := n.(*ast.SelectorExpr)
		if ok {
			ident, ok := seln.X.(*ast.Ident)
			if ok {
				if ident.Name == evalop.BreakpointHitCountVarNamePackage && seln.Sel.Name == evalop.BreakpointHitCountVarName {
					r = true
					return false
				}
			}
		}
		return true
	})
	return r
}
