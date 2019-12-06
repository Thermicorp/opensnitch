package ebpf

// execve probe using ebpf

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"unsafe"

	bpf "github.com/iovisor/gobpf/bcc"
)

import "C"

type EventType int32

const (
	eventArg EventType = iota
	eventRet
)


const source string = `
#include <uapi/linux/ptrace.h>
#include <linux/sched.h>
#include <linux/fs.h>
#define ARGSIZE  128
enum event_type {
    EVENT_ARG,
    EVENT_RET,
};
struct data_t {
    u64 pid;  // PID as in the userspace term (i.e. task->tgid in kernel)
    u64 ppid; // Parent PID as in the userspace term (i.e task->real_parent->tgid in kernel)
    char comm[TASK_COMM_LEN];
    enum event_type type;
    char argv[ARGSIZE];
    int retval;
};
BPF_PERF_OUTPUT(events);
static int __submit_arg(struct pt_regs *ctx, void *ptr, struct data_t *data)
{
    bpf_probe_read(data->argv, sizeof(data->argv), ptr);
    events.perf_submit(ctx, data, sizeof(struct data_t));
    return 1;
}
static int submit_arg(struct pt_regs *ctx, void *ptr, struct data_t *data)
{
    const char *argp = NULL;
    bpf_probe_read(&argp, sizeof(argp), ptr);
    if (argp) {
        return __submit_arg(ctx, (void *)(argp), data);
    }
    return 0;
}
int syscall__execve(struct pt_regs *ctx,
    const char __user *filename,
    const char __user *const __user *__argv,
    const char __user *const __user *__envp)
{
    // create data here and pass to submit_arg to save stack space (#555)
    struct data_t data = {};
    struct task_struct *task;
    data.pid = bpf_get_current_pid_tgid() >> 32;
    task = (struct task_struct *)bpf_get_current_task();
    // Some kernels, like Ubuntu 4.13.0-generic, return 0
    // as the real_parent->tgid.
    // We use the getPpid function as a fallback in those cases.
    // See https://github.com/iovisor/bcc/issues/1883.
    data.ppid = task->real_parent->tgid;
    bpf_get_current_comm(&data.comm, sizeof(data.comm));
    data.type = EVENT_ARG;
    __submit_arg(ctx, (void *)filename, &data);
    // skip first arg, as we submitted filename
    #pragma unroll
    for (int i = 1; i < MAX_ARGS; i++) {
        if (submit_arg(ctx, (void *)&__argv[i], &data) == 0)
             goto out;
    }
    // handle truncated argument list
    char ellipsis[] = "...";
    __submit_arg(ctx, (void *)ellipsis, &data);
out:
    return 0;
}
int do_ret_sys_execve(struct pt_regs *ctx)
{
    struct data_t data = {};
    struct task_struct *task;
    data.pid = bpf_get_current_pid_tgid() >> 32;
    task = (struct task_struct *)bpf_get_current_task();
    // Some kernels, like Ubuntu 4.13.0-generic, return 0
    // as the real_parent->tgid.
    // We use the getPpid function as a fallback in those cases.
    // See https://github.com/iovisor/bcc/issues/1883.
    data.ppid = task->real_parent->tgid;
    bpf_get_current_comm(&data.comm, sizeof(data.comm));
    data.type = EVENT_RET;
    data.retval = PT_REGS_RC(ctx);
    events.perf_submit(ctx, &data, sizeof(data));
    return 0;
}
`


type execveEvent struct {
	Pid    uint64
	Ppid   uint64
	Comm   [16]byte
	Type   int32
	Argv   [128]byte
	RetVal int32
}

type eventPayload struct {
	Time   string `json:"time,omitempty"`
	Comm   string `json:"comm"`
	Pid    uint64 `json:"pid"`
	Ppid   string `json:"ppid"`
	Argv   string `json:"argv"`
	RetVal int32  `json:"retval"`
}

var inUse bool
var bpfModule *bpf.Module
var perfMap *bpf.PerfMap

// getPpid is a fallback to read the parent PID from /proc.
// Some kernel versions, like 4.13.0 return 0 getting the parent PID
// from the current task, so we need to use this fallback to have
// the parent PID in any kernel.
func getPpid(pid uint64) uint64 {
	f, err := os.OpenFile(fmt.Sprintf("/proc/%d/status", pid), os.O_RDONLY, os.ModePerm)
	if err != nil {
		return 0
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		text := sc.Text()
		if strings.Contains(text, "PPid:") {
			f := strings.Fields(text)
			i, _ := strconv.ParseUint(f[len(f)-1], 10, 64)
			return i
		}
	}
	return 0
}
func destroyProbe() {
	if inUse {
		bpfModule.close()
		perfMap.Stop()
	}
	inUse = false
}
func createProbe(eventChannel chan eventPayload, argNum uint32) error {

	m = bpf.NewModule(strings.Replace(source, "MAX_ARGS", strconv.FormatUint(*maxArgs, 10), -1), []string{})
	defer m.Close()

	fnName := bpf.GetSyscallFnName("execve")

	kprobe, err := m.LoadKprobe("syscall__execve")
	if err != nil {
		return errors.New("Failed to load syscall__execve: %s\n", err)
	}

	// passing -1 for maxActive signifies to use the default
	// according to the kernel kprobes documentation
	if err := m.AttachKprobe(fnName, kprobe, -1); err != nil {
		return errors.New("Failed to attach syscall__execve: %s\n", err)
	}

	kretprobe, err := m.LoadKprobe("do_ret_sys_execve")
	if err != nil {
		return errors.New("Failed to load do_ret_sys_execve: %s\n", err)
	}

	// passing -1 for maxActive signifies to use the default
	// according to the kernel kretprobes documentation
	if err := m.AttachKretprobe(fnName, kretprobe, -1); err != nil {
		return errors.New("Failed to attach do_ret_sys_execve: %s\n", err)
	}

	table = bpf.NewTable(m.TableId("events"), m)

	channel := make(chan []byte, 1000)

	perfMap, err := bpf.InitPerfMap(table, channel)
	if err != nil {
		return errors.New("Failed to init perf map: %s\n", err)
	}
	inUse = true
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, os.Kill)

	go func() {
		out := newOutput(*format, *pretty, *timestamps)
		out.PrintHeader()

		args := make(map[uint64][]string)

		for {
			data := <-channel

			var event execveEvent
			err := binary.Read(bytes.NewBuffer(data), bpf.GetHostByteOrder(), &event)

			if err != nil {
				log.Warning("failed to decode received data: %s\n", err)
				continue
			}

			if eventArg == EventType(event.Type) {
				e, ok := args[event.Pid]
				if !ok {
					e = make([]string, 0)
				}
				argv := (*C.char)(unsafe.Pointer(&event.Argv))

				e = append(e, C.GoString(argv))
				args[event.Pid] = e
			} else {
				if event.RetVal != 0 && !*traceFailed {
					delete(args, event.Pid)
					continue
				}

				comm := C.GoString((*C.char)(unsafe.Pointer(&event.Comm)))
				if *filterComm != "" && !strings.Contains(comm, *filterComm) {
					delete(args, event.Pid)
					continue
				}

				argv, ok := args[event.Pid]
				if !ok {
					continue
				}

				if *filterArg != "" && !strings.Contains(strings.Join(argv, " "), *filterArg) {
					delete(args, event.Pid)
					continue
				}

				p := eventPayload{
					Pid:    event.Pid,
					Ppid:   "?",
					Comm:   comm,
					RetVal: event.RetVal,
				}

				if event.Ppid == 0 {
					event.Ppid = getPpid(event.Pid)
				}

				if event.Ppid != 0 {
					p.Ppid = strconv.FormatUint(event.Ppid, 10)
				}

				var b bytes.Buffer
				for i, a := range argv {
					b.WriteString(strings.Replace(a, `"`, `\"`, -1))
					if i != len(argv)-1 {
						b.WriteString(" ")
					}
				}
				p.Argv = b.String()
				p.Argv = strings.TrimSpace(strings.Replace(p.Argv, "\n", "\\n", -1))
				eventChannel <- p
				delete(args, event.Pid)
			}
		}
	}()
	perfMap.Start()
	return true
}