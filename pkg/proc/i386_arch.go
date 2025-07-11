package proc

import (
	"encoding/binary"
	"fmt"
	"strings"

	"github.com/go-delve/delve/pkg/dwarf/frame"
	"github.com/go-delve/delve/pkg/dwarf/op"
	"github.com/go-delve/delve/pkg/dwarf/regnum"
)

var i386BreakInstruction = []byte{0xCC}

// I386Arch returns an initialized I386Arch
// struct.
func I386Arch(goos string) *Arch {
	return &Arch{
		Name:                             "386",
		ptrSize:                          4,
		maxInstructionLength:             15,
		breakpointInstruction:            i386BreakInstruction,
		altBreakpointInstruction:         []byte{0xcd, 0x03},
		breakInstrMovesPC:                true,
		derefTLS:                         false,
		prologues:                        prologuesI386,
		fixFrameUnwindContext:            i386FixFrameUnwindContext,
		switchStack:                      i386SwitchStack,
		regSize:                          i386RegSize,
		RegistersToDwarfRegisters:        i386RegistersToDwarfRegisters,
		addrAndStackRegsToDwarfRegisters: i386AddrAndStackRegsToDwarfRegisters,
		DwarfRegisterToString:            i386DwarfRegisterToString,
		inhibitStepInto:                  i386InhibitStepInto,
		asmDecode:                        i386AsmDecode,
		PCRegNum:                         regnum.I386_Eip,
		SPRegNum:                         regnum.I386_Esp,
		asmRegisters:                     i386AsmRegisters,
		RegisterNameToDwarf:              nameToDwarfFunc(regnum.I386NameToDwarf),
		RegnumToString:                   regnum.I386ToName,
	}
}

func i386FixFrameUnwindContext(fctxt *frame.FrameContext, pc uint64, bi *BinaryInfo) *frame.FrameContext {
	i := bi.Arch
	if i.sigreturnfn == nil {
		i.sigreturnfn = bi.lookupOneFunc("runtime.sigreturn")
	}

	if fctxt == nil || (i.sigreturnfn != nil && pc >= i.sigreturnfn.Entry && pc < i.sigreturnfn.End) {
		// When there's no frame descriptor entry use BP (the frame pointer) instead
		// - return register is [bp + i.PtrSize()] (i.e. [cfa-i.PtrSize()])
		// - cfa is bp + i.PtrSize()*2
		// - bp is [bp] (i.e. [cfa-i.PtrSize()*2])
		// - sp is cfa

		// When the signal handler runs it will move the execution to the signal
		// handling stack (installed using the sigaltstack system call).
		// This isn't i proper stack switch: the pointer to g in TLS will still
		// refer to whatever g was executing on that thread before the signal was
		// received.
		// Since go did not execute i stack switch the previous value of sp, pc
		// and bp is not saved inside g.sched, as it normally would.
		// The only way to recover is to either read sp/pc from the signal context
		// parameter (the ucontext_t* parameter) or to unconditionally follow the
		// frame pointer when we get to runtime.sigreturn (which is what we do
		// here).

		return &frame.FrameContext{
			RetAddrReg: regnum.I386_Eip,
			Regs: map[uint64]frame.DWRule{
				regnum.I386_Eip: {
					Rule:   frame.RuleOffset,
					Offset: int64(-i.PtrSize()),
				},
				regnum.I386_Ebp: {
					Rule:   frame.RuleOffset,
					Offset: int64(-2 * i.PtrSize()),
				},
				regnum.I386_Esp: {
					Rule:   frame.RuleValOffset,
					Offset: 0,
				},
			},
			CFA: frame.DWRule{
				Rule:   frame.RuleCFA,
				Reg:    regnum.I386_Ebp,
				Offset: int64(2 * i.PtrSize()),
			},
		}
	}

	if i.crosscall2fn == nil {
		i.crosscall2fn = bi.lookupOneFunc("crosscall2")
	}

	// TODO(chainhelen), need to check whether there is a bad frame descriptor like amd64.
	// crosscall2 is defined in $GOROOT/src/runtime/cgo/asm_386.s.
	if i.crosscall2fn != nil && pc >= i.crosscall2fn.Entry && pc < i.crosscall2fn.End {
		rule := fctxt.CFA
		fctxt.CFA = rule
	}

	// We assume that EBP is the frame pointer and we want to keep it updated,
	// so that we can use it to unwind the stack even when we encounter frames
	// without descriptor entries.
	// If there isn't i rule already we emit one.
	if fctxt.Regs[regnum.I386_Ebp].Rule == frame.RuleUndefined {
		fctxt.Regs[regnum.I386_Ebp] = frame.DWRule{
			Rule:   frame.RuleFramePointer,
			Reg:    regnum.I386_Ebp,
			Offset: 0,
		}
	}

	return fctxt
}

// SwitchStack will use the current frame to determine if it's time to
func i386SwitchStack(it *stackIterator, _ *op.DwarfRegisters) bool {
	if it.frame.Current.Fn == nil {
		if it.systemstack && it.g != nil && it.top {
			if err := it.switchToGoroutineStack(); err != nil {
				it.err = err
				return false
			}
			return true
		}
		return false
	}
	switch it.frame.Current.Fn.Name {
	case "runtime.asmcgocall", "runtime.cgocallback_gofunc": // TODO(chainhelen), need to support cgo stacktraces.
		return false
	case "runtime.goexit", "runtime.rt0_go":
		// Look for "top of stack" functions.
		it.atend = true
		return true

	case "runtime.mcall":
		if it.systemstack && it.g != nil {
			if err := it.switchToGoroutineStack(); err != nil {
				it.err = err
				return false
			}
			return true
		}
		it.atend = true
		return true

	case "runtime.mstart":
		// Calls to runtime.systemstack will switch to the systemstack then:
		// 1. alter the goroutine stack so that it looks like systemstack_switch
		//    was called
		// 2. alter the system stack so that it looks like the bottom-most frame
		//    belongs to runtime.mstart
		// If we find a runtime.mstart frame on the system stack of a goroutine
		// parked on runtime.systemstack_switch we assume runtime.systemstack was
		// called and continue tracing from the parked position.

		if it.top || !it.systemstack || it.g == nil {
			return false
		}
		if fn := it.bi.PCToFunc(it.g.PC); fn == nil || fn.Name != "runtime.systemstack_switch" {
			return false
		}

		if err := it.switchToGoroutineStack(); err != nil {
			it.err = err
			return false
		}
		return true

	case "runtime.newstack", "runtime.systemstack":
		if it.systemstack && it.g != nil {
			if err := it.switchToGoroutineStack(); err != nil {
				it.err = err
				return false
			}
			return true
		}

		return false

	default:
		return false
	}
}

// RegSize returns the size (in bytes) of register regnum.
// The mapping between hardware registers and DWARF registers is specified
// in the System V ABI Intel386 Architecture Processor Supplement page 25,
// table 2.14
// https://www.uclibc.org/docs/psABI-i386.pdf
func i386RegSize(regnum uint64) int {
	// XMM registers
	if regnum >= 21 && regnum <= 36 {
		return 16
	}
	// x87 registers
	if regnum >= 11 && regnum <= 18 {
		return 10
	}
	return 4
}

func i386RegistersToDwarfRegisters(staticBase uint64, regs Registers) *op.DwarfRegisters {
	dregs := initDwarfRegistersFromSlice(regnum.I386MaxRegNum(), regs, regnum.I386NameToDwarf)
	dr := op.NewDwarfRegisters(staticBase, dregs, binary.LittleEndian, regnum.I386_Eip, regnum.I386_Esp, regnum.I386_Ebp, 0)
	dr.SetLoadMoreCallback(loadMoreDwarfRegistersFromSliceFunc(dr, regs, regnum.I386NameToDwarf))

	return dr
}

func i386AddrAndStackRegsToDwarfRegisters(staticBase, pc, sp, bp, lr uint64) op.DwarfRegisters {
	dregs := make([]*op.DwarfRegister, regnum.I386_Eip+1)
	dregs[regnum.I386_Eip] = op.DwarfRegisterFromUint64(pc)
	dregs[regnum.I386_Esp] = op.DwarfRegisterFromUint64(sp)
	dregs[regnum.I386_Ebp] = op.DwarfRegisterFromUint64(bp)

	return *op.NewDwarfRegisters(staticBase, dregs, binary.LittleEndian, regnum.I386_Eip, regnum.I386_Esp, regnum.I386_Ebp, 0)
}

func i386DwarfRegisterToString(j int, reg *op.DwarfRegister) (name string, floatingPoint bool, repr string) {
	name = regnum.I386ToName(uint64(j))

	if reg == nil {
		return name, false, ""
	}

	switch n := strings.ToLower(name); n {
	case "eflags":
		return name, false, eflagsDescription.Describe(reg.Uint64Val, 32)

	case "tw", "fop":
		return name, true, fmt.Sprintf("%#04x", reg.Uint64Val)

	default:
		if reg.Bytes != nil && strings.HasPrefix(n, "xmm") {
			return name, true, formatSSEReg(name, reg.Bytes)
		} else if reg.Bytes != nil && strings.HasPrefix(n, "st(") {
			return name, true, formatX87Reg(reg.Bytes)
		} else if reg.Bytes == nil || (reg.Bytes != nil && len(reg.Bytes) <= 8) {
			return name, false, fmt.Sprintf("%#016x", reg.Uint64Val)
		} else {
			return name, false, fmt.Sprintf("%#x", reg.Bytes)
		}
	}
}

// i386InhibitStepInto returns whether StepBreakpoint can be set at pc.
// When cgo or pie on 386 linux, compiler will insert more instructions (ex: call __x86.get_pc_thunk.).
// StepBreakpoint shouldn't be set on __x86.get_pc_thunk and skip it.
// See comments on stacksplit in $GOROOT/src/cmd/internal/obj/x86/obj6.go for generated instructions details.
func i386InhibitStepInto(bi *BinaryInfo, pc uint64) bool {
	if bi.SymNames != nil && bi.SymNames[pc] != nil &&
		strings.HasPrefix(bi.SymNames[pc].Name, "__x86.get_pc_thunk.") {
		return true
	}
	return false
}
