//go:build !windows

package link

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"unsafe"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/asm"
	"github.com/cilium/ebpf/internal"
	"github.com/cilium/ebpf/internal/sys"
	"github.com/cilium/ebpf/internal/tracefs"
	"github.com/cilium/ebpf/internal/unix"
)

// Getting the terminology right is usually the hardest part. For posterity and
// for staying sane during implementation:
//
// - trace event: Representation of a kernel runtime hook. Filesystem entries
//   under <tracefs>/events. Can be tracepoints (static), kprobes or uprobes.
//   Can be instantiated into perf events (see below).
// - tracepoint: A predetermined hook point in the kernel. Exposed as trace
//   events in (sub)directories under <tracefs>/events. Cannot be closed or
//   removed, they are static.
// - k(ret)probe: Ephemeral trace events based on entry or exit points of
//   exported kernel symbols. kprobe-based (tracefs) trace events can be
//   created system-wide by writing to the <tracefs>/kprobe_events file, or
//   they can be scoped to the current process by creating PMU perf events.
// - u(ret)probe: Ephemeral trace events based on user provides ELF binaries
//   and offsets. uprobe-based (tracefs) trace events can be
//   created system-wide by writing to the <tracefs>/uprobe_events file, or
//   they can be scoped to the current process by creating PMU perf events.
// - perf event: An object instantiated based on an existing trace event or
//   kernel symbol. Referred to by fd in userspace.
//   Exactly one eBPF program can be attached to a perf event. Multiple perf
//   events can be created from a single trace event. Closing a perf event
//   stops any further invocations of the attached eBPF program.

var (
	errInvalidInput = tracefs.ErrInvalidInput
)

const (
	perfAllThreads = -1
)

// A perfEvent represents a perf event kernel object. Exactly one eBPF program
// can be attached to it. It is created based on a tracefs trace event or a
// Performance Monitoring Unit (PMU).
type perfEvent struct {
	// Trace event backing this perfEvent. May be nil.
	tracefsEvent *tracefs.Event

	// This is the perf event FD.
	fd *sys.FD
}

func newPerfEvent(fd *sys.FD, event *tracefs.Event) *perfEvent {
	pe := &perfEvent{event, fd}
	// Both event and fd have their own finalizer, but we want to
	// guarantee that they are closed in a certain order.
	runtime.SetFinalizer(pe, (*perfEvent).Close)
	return pe
}

func (pe *perfEvent) Close() error {
	runtime.SetFinalizer(pe, nil)

	if err := pe.fd.Close(); err != nil {
		return fmt.Errorf("closing perf event fd: %w", err)
	}

	if pe.tracefsEvent != nil {
		return pe.tracefsEvent.Close()
	}

	return nil
}

// PerfEvent is implemented by some Link types which use a perf event under
// the hood.
type PerfEvent interface {
	// PerfEvent returns a file for the underlying perf event.
	//
	// It is the callers responsibility to close the returned file.
	//
	// Making changes to the associated perf event lead to
	// undefined behaviour.
	PerfEvent() (*os.File, error)
}

// perfEventLink represents a bpf perf link.
type perfEventLink struct {
	RawLink
	pe *perfEvent
}

func (pl *perfEventLink) isLink() {}

func (pl *perfEventLink) Close() error {
	if err := pl.fd.Close(); err != nil {
		return fmt.Errorf("perf link close: %w", err)
	}

	// when created from pinned link
	if pl.pe == nil {
		return nil
	}

	if err := pl.pe.Close(); err != nil {
		return fmt.Errorf("perf event close: %w", err)
	}
	return nil
}

func (pl *perfEventLink) Update(_ *ebpf.Program) error {
	return fmt.Errorf("perf event link update: %w", ErrNotSupported)
}

var _ PerfEvent = (*perfEventLink)(nil)

func (pl *perfEventLink) PerfEvent() (*os.File, error) {
	// when created from pinned link
	if pl.pe == nil {
		return nil, ErrNotSupported
	}

	fd, err := pl.pe.fd.Dup()
	if err != nil {
		return nil, err
	}

	return fd.File("perf-event")
}

func (pl *perfEventLink) Info() (*Info, error) {
	var info sys.PerfEventLinkInfo
	if err := sys.ObjInfo(pl.fd, &info); err != nil {
		return nil, fmt.Errorf("perf event link info: %s", err)
	}

	var extra2 interface{}
	switch info.PerfEventType {
	case sys.BPF_PERF_EVENT_KPROBE, sys.BPF_PERF_EVENT_KRETPROBE:
		var kprobeInfo sys.KprobeLinkInfo
		if err := sys.ObjInfo(pl.fd, &kprobeInfo); err != nil {
			return nil, fmt.Errorf("kprobe link info: %s", err)
		}
		extra2 = &KprobeInfo{
			address: kprobeInfo.Addr,
			missed:  kprobeInfo.Missed,
		}
	}

	extra := &PerfEventInfo{
		Type:  info.PerfEventType,
		extra: extra2,
	}

	return &Info{
		info.Type,
		info.Id,
		ebpf.ProgramID(info.ProgId),
		extra,
	}, nil
}

// perfEventIoctl implements Link and handles the perf event lifecycle
// via ioctl().
type perfEventIoctl struct {
	*perfEvent
}

func (pi *perfEventIoctl) isLink() {}

// Since 4.15 (e87c6bc3852b "bpf: permit multiple bpf attachments for a single perf event"),
// calling PERF_EVENT_IOC_SET_BPF appends the given program to a prog_array
// owned by the perf event, which means multiple programs can be attached
// simultaneously.
//
// Before 4.15, calling PERF_EVENT_IOC_SET_BPF more than once on a perf event
// returns EEXIST.
//
// Detaching a program from a perf event is currently not possible, so a
// program replacement mechanism cannot be implemented for perf events.
func (pi *perfEventIoctl) Update(_ *ebpf.Program) error {
	return fmt.Errorf("perf event ioctl update: %w", ErrNotSupported)
}

func (pi *perfEventIoctl) Pin(string) error {
	return fmt.Errorf("perf event ioctl pin: %w", ErrNotSupported)
}

func (pi *perfEventIoctl) Unpin() error {
	return fmt.Errorf("perf event ioctl unpin: %w", ErrNotSupported)
}

func (pi *perfEventIoctl) Info() (*Info, error) {
	return nil, fmt.Errorf("perf event ioctl info: %w", ErrNotSupported)
}

var _ PerfEvent = (*perfEventIoctl)(nil)

func (pi *perfEventIoctl) PerfEvent() (*os.File, error) {
	fd, err := pi.fd.Dup()
	if err != nil {
		return nil, err
	}

	return fd.File("perf-event")
}

// attach the given eBPF prog to the perf event stored in pe.
// pe must contain a valid perf event fd.
// prog's type must match the program type stored in pe.
func attachPerfEvent(pe *perfEvent, prog *ebpf.Program, cookie uint64) (Link, error) {
	if prog == nil {
		return nil, errors.New("cannot attach a nil program")
	}
	if prog.FD() < 0 {
		return nil, fmt.Errorf("invalid program: %w", sys.ErrClosedFd)
	}

	if err := haveBPFLinkPerfEvent(); err == nil {
		return attachPerfEventLink(pe, prog, cookie)
	}

	if cookie != 0 {
		return nil, fmt.Errorf("cookies are not supported: %w", ErrNotSupported)
	}

	return attachPerfEventIoctl(pe, prog)
}

func attachPerfEventIoctl(pe *perfEvent, prog *ebpf.Program) (*perfEventIoctl, error) {
	// Assign the eBPF program to the perf event.
	err := unix.IoctlSetInt(pe.fd.Int(), unix.PERF_EVENT_IOC_SET_BPF, prog.FD())
	if err != nil {
		return nil, fmt.Errorf("setting perf event bpf program: %w", err)
	}

	// PERF_EVENT_IOC_ENABLE and _DISABLE ignore their given values.
	if err := unix.IoctlSetInt(pe.fd.Int(), unix.PERF_EVENT_IOC_ENABLE, 0); err != nil {
		return nil, fmt.Errorf("enable perf event: %s", err)
	}

	return &perfEventIoctl{pe}, nil
}

// Use the bpf api to attach the perf event (BPF_LINK_TYPE_PERF_EVENT, 5.15+).
//
// https://github.com/torvalds/linux/commit/b89fbfbb854c9afc3047e8273cc3a694650b802e
func attachPerfEventLink(pe *perfEvent, prog *ebpf.Program, cookie uint64) (*perfEventLink, error) {
	fd, err := sys.LinkCreatePerfEvent(&sys.LinkCreatePerfEventAttr{
		ProgFd:     uint32(prog.FD()),
		TargetFd:   pe.fd.Uint(),
		AttachType: sys.BPF_PERF_EVENT,
		BpfCookie:  cookie,
	})
	if err != nil {
		return nil, fmt.Errorf("cannot create bpf perf link: %v", err)
	}

	return &perfEventLink{RawLink{fd: fd}, pe}, nil
}

// unsafeStringPtr returns an unsafe.Pointer to a NUL-terminated copy of str.
func unsafeStringPtr(str string) (unsafe.Pointer, error) {
	p, err := unix.BytePtrFromString(str)
	if err != nil {
		return nil, err
	}
	return unsafe.Pointer(p), nil
}

// openTracepointPerfEvent opens a tracepoint-type perf event. System-wide
// [k,u]probes created by writing to <tracefs>/[k,u]probe_events are tracepoints
// behind the scenes, and can be attached to using these perf events.
func openTracepointPerfEvent(tid uint64, pid int) (*sys.FD, error) {
	attr := unix.PerfEventAttr{
		Type:        unix.PERF_TYPE_TRACEPOINT,
		Config:      tid,
		Sample_type: unix.PERF_SAMPLE_RAW,
		Sample:      1,
		Wakeup:      1,
	}

	cpu := 0
	if pid != perfAllThreads {
		cpu = -1
	}
	fd, err := unix.PerfEventOpen(&attr, pid, cpu, -1, unix.PERF_FLAG_FD_CLOEXEC)
	if err != nil {
		return nil, fmt.Errorf("opening tracepoint perf event: %w", err)
	}

	return sys.NewFD(fd)
}

// Probe BPF perf link.
//
// https://elixir.bootlin.com/linux/v5.16.8/source/kernel/bpf/syscall.c#L4307
// https://github.com/torvalds/linux/commit/b89fbfbb854c9afc3047e8273cc3a694650b802e
var haveBPFLinkPerfEvent = internal.NewFeatureTest("bpf_link_perf_event", func() error {
	prog, err := ebpf.NewProgram(&ebpf.ProgramSpec{
		Name: "probe_bpf_perf_link",
		Type: ebpf.Kprobe,
		Instructions: asm.Instructions{
			asm.Mov.Imm(asm.R0, 0),
			asm.Return(),
		},
		License: "MIT",
	})
	if err != nil {
		return err
	}
	defer prog.Close()

	_, err = sys.LinkCreatePerfEvent(&sys.LinkCreatePerfEventAttr{
		ProgFd:     uint32(prog.FD()),
		AttachType: sys.BPF_PERF_EVENT,
	})
	if errors.Is(err, unix.EINVAL) {
		return internal.ErrNotSupported
	}
	if errors.Is(err, unix.EBADF) {
		return nil
	}
	return err
}, "5.15")
