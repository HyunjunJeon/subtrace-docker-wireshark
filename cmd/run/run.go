// Copyright (c) Subtrace, Inc.
// SPDX-License-Identifier: BSD-3-Clause

package run

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/pprof"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/google/martian/v3/log"
	"github.com/peterbourgon/ff/v3"
	"github.com/peterbourgon/ff/v3/ffcli"
	"golang.org/x/sys/unix"
	"subtrace.dev/cmd/run/engine"
	"subtrace.dev/cmd/run/engine/process"
	"subtrace.dev/cmd/run/engine/seccomp"
	"subtrace.dev/cmd/run/fd"
	"subtrace.dev/cmd/run/futex"
	"subtrace.dev/cmd/run/journal"
	"subtrace.dev/cmd/run/kernel"
	"subtrace.dev/cmd/run/socket"
	"subtrace.dev/cmd/run/tls"
	"subtrace.dev/cmd/version"
	"subtrace.dev/config"
	"subtrace.dev/devtools"
	"subtrace.dev/global"
	"subtrace.dev/logging"
	"subtrace.dev/stats"
	"subtrace.dev/tracer"
)

type Command struct {
	ffcli.Command
	flags struct {
		log      *bool
		pprof    string
		devtools string
		config   string
	}

	global *global.Global
}

func NewCommand() *ffcli.Command {
	c := new(Command)

	c.Name = "run"
	c.ShortUsage = "subtrace run [flags] -- <command> [arguments]"
	c.ShortHelp = "run a command with subtrace"

	c.FlagSet = flag.NewFlagSet(filepath.Base(os.Args[0]), flag.ContinueOnError)
	c.flags.log = c.FlagSet.Bool("log", false, "log trace events to stderr")
	c.FlagSet.Int64Var(&tracer.PayloadLimitBytes, "payload-limit", 4096, "payload size limit in bytes after which request/response body will be truncated")
	c.FlagSet.StringVar(&c.flags.config, "config", "", "configuration file path")
	c.FlagSet.StringVar(&c.flags.devtools, "devtools", "", "path to serve the chrome devtools bundle on")
	c.FlagSet.BoolVar(&tls.Enabled, "tls", true, "intercept outgoing TLS requests")
	c.FlagSet.StringVar(&c.flags.pprof, "pprof", "", "write pprof CPU profile to file")
	c.FlagSet.BoolVar(&journal.Enabled, "tracelogs", false, "trace stdout and stderr logs")
	c.FlagSet.BoolVar(&logging.Verbose, "v", false, "enable verbose debug logging")
	c.FlagSet.StringVar(&logging.Logfile, "logfile", "", "file for debug logs (stdout if unspecified)")
	c.UsageFunc = func(fc *ffcli.Command) string {
		return ffcli.DefaultUsageFunc(fc) + ExtraHelp()
	}

	c.Options = []ff.Option{ff.WithEnvVarPrefix("SUBTRACE")}
	c.Exec = c.entrypoint
	return &c.Command
}

func ExtraHelp() string {
	ok := false
	if len(os.Args) <= 2 {
		ok = true
	} else {
		for _, arg := range os.Args {
			if arg == "--" {
				break
			}
			if arg == "-h" || arg == "-help" || arg == "--help" {
				ok = true
				break
			}
		}
	}
	if !ok {
		return ""
	}

	return strings.Join([]string{
		"",
		"EXAMPLES",
		"  $ subtrace run -- nginx",
		"  $ subtrace run -- python -m http.server",
		"  $ subtrace run -- curl https://subtrace.dev",
		"",
		"MORE",
		"  https://docs.subtrace.dev",
		"  https://subtrace.dev/discord",
		"",
	}, "\n")
}

func (c *Command) entrypoint(ctx context.Context, args []string) error {
	if err := logging.Init(); err != nil {
		return fmt.Errorf("init logging: %w", err)
	}

	if len(args) == 0 {
		// Log to stdout so that the usage and help text is greppable (see [1]).
		// [1] https://news.ycombinator.com/item?id=37682859
		c.FlagSet.SetOutput(os.Stdout)
		c.FlagSet.Usage()
		return nil
	}

	slog.Debug("starting tracer", "parent", os.Getenv("_SUBTRACE_CHILD") == "", "release", version.Release, slog.Group("commit", "hash", version.CommitHash, "time", version.CommitTime), "build", version.BuildTime)

	switch os.Getenv("_SUBTRACE_CHILD") {
	case "": // parent
		code, err := c.entrypointParent(ctx, args)
		switch {
		case err == nil:
			slog.Debug("parent exiting", "code", code)
			os.Exit(code)

		case errors.Is(err, errMissingCommand):
			if os.Args[len(os.Args)-1] == "--" {
				fmt.Fprintf(os.Stderr, "subtrace: error: missing COMMAND\n")
			}
			os.Exit(1)

		case errors.Is(err, kernel.ErrUnsupportedVersion):
			major, minor, _ := kernel.CheckVersion(minKernelVersion, false)
			fmt.Fprintf(os.Stderr, "subtrace: error: unsupported Linux kernel version (got %d.%d, want %s+)\n", major, minor, minKernelVersion)
			os.Exit(1)

		default:
			slog.Debug("parent exiting", "code", code, "err", err)
			return fmt.Errorf("parent: %w", err)
		}

	default: // child
		os.Unsetenv("_SUBTRACE_CHILD")
		if err := c.entrypointChild(ctx, args); err != nil {
			fmt.Fprintf(os.Stderr, "child: %v\n", err)
			os.Exit(1)
		}
	}
	panic("unreachable")
}

// ensureAsyncPreemptionHack checks if GODEBUG=asyncpreemptoff=1 is set. If not
// found, it restarts the subtrace parent process with the value set.
//
// Async preemption is a Go 1.14+ feature that allows the Go runtime scheduler
// to send a SIGURG signal to preempt goroutines. It's enabled by default and
// is normally useful because it allows for preempting long running goroutines
// like tight loops that do not have yield points like function calls.
//
// Unfortunately, async preemption seems to mess with our ioctl(2) calls that
// do SECCOMP_ADDFD_FLAG_SEND: if the Go scheduler preempts an OS thread that's
// in the middle of a seccomp_unotify(2) ioctl call installling a socket into
// the target's file descriptor table, due to some unknown bug in either the
// Linux kernel (likely) or the Go scheduler (unlikely), the atomicity of the
// operation gets violated. SECCOMP_ADDFD_FLAG_SEND is supposed to atomically
// install the file into the tracee's file descriptor table, mark the seccomp
// notification as complete and set the installed file descriptor number as the
// tracee's syscall return value, but it unexpectedly results in the tracee
// seeing 0 as the syscall return value and subtrace's ioctl call returning
// EINTR. Since 0 is a valid file descriptor number, the tracee thinks its
// socket(2) or accept(2) syscall succeeded and 0 is the new socket file
// descriptor. But this is wrong and 0 is usually standard input (and not a
// socket), so subsequent socket operations will fail with ENOTSOCK. Moreover,
// operating on the wrong file descriptor can be catastrophic.
//
// To work around this issue, we use GODEBUG to disable async preemption when
// initializing the parent process (we reset it back to its original value
// before executing the tracee so that we don't change its behavior in case
// it's also a Go program). This ensures that the Go scheduler will never
// interrupt any syscall with SIGURG.
//
// TODO(adtac): bisect the earliest Go and Linux versions this happens in
// TODO(adtac): does this also happen on linux/amd64? (tested on arm64)
func (c *Command) ensureAsyncPreemptionHack() error {
	orig := os.Getenv("GODEBUG")

	var excl []string
	for _, kv := range strings.Split(orig, ",") {
		k, v, _ := strings.Cut(kv, "=")
		if k != "asyncpreemptoff" {
			excl = append(excl, kv)
			continue
		}
		if v == "1" {
			slog.Debug("asyncpreemptoff=1 found", "GODEBUG", os.Getenv("GODEBUG"), "SUBTRACE_ORIG_GODEBUG", os.Getenv("SUBTRACE_ORIG_GODEBUG"))
			switch prev := os.Getenv("SUBTRACE_ORIG_GODEBUG"); prev {
			case "<empty>":
				os.Unsetenv("SUBTRACE_ORIG_GODEBUG")
				os.Unsetenv("GODEBUG")
			case "":
			default:
				os.Unsetenv("SUBTRACE_ORIG_GODEBUG")
				os.Setenv("GODEBUG", prev)
			}
			return nil
		}
	}

	slog.Debug("asyncpreemptoff=1 not found, restarting", "GODEBUG", os.Getenv("GODEBUG"))
	var environ []string
	for _, kv := range os.Environ() {
		switch {
		case strings.HasPrefix(kv, "GODEBUG="):
			environ = append(environ, "GODEBUG="+strings.Join(append(excl, "asyncpreemptoff=1"), ","))
		default:
			environ = append(environ, kv)
		}
	}

	if orig != "" {
		// We don't want to propagate the hack to child processes that might also
		// be Go programs, so we temporarily store the original value and restore
		// it after execve.
		environ = append(environ, "SUBTRACE_ORIG_GODEBUG="+orig)
	} else {
		environ = append(environ, "GODEBUG=asyncpreemptoff=1")
		environ = append(environ, "SUBTRACE_ORIG_GODEBUG=<empty>")
	}
	abspath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("executable: %w", err)
	}
	if err := unix.Exec(abspath, os.Args, environ); err != nil {
		return fmt.Errorf("execve: %w", err)
	}
	return nil
}

var errMissingCommand = fmt.Errorf("missing COMMAND")

const (
	// required >= 5.0  (2019-03-03) for seccom_unotify(2)
	// required >= 5.3  (2019-09-15) for pidfd_open(2)
	// required >= 5.6  (2020-03-29) for pidfd_getfd(2)
	// required >= 5.7  (2020-05-31) for SECCOMP_FILTER_FLAG_TSYNC_ESRCH
	// required >= 5.9  (2020-10-11) for SECCOMP_IOCTL_NOTIF_ADDFD
	// required >= 5.14 (2021-08-29) for SECCOMP_ADDFD_FLAG_SEND
	//          >= 5.19 (2022-07-31) for SECCOMP_FILTER_FLAG_WAIT_KILLABLE_RECV
	minKernelVersion = "5.14"
)

func (c *Command) entrypointParent(ctx context.Context, args []string) (int, error) {
	if len(args) == 0 {
		return 0, errMissingCommand
	}

	if err := c.ensureAsyncPreemptionHack(); err != nil {
		return 0, fmt.Errorf("ensure asyncpreemptoff=1: %w", err)
	}

	slog.Debug("starting tracer parent", "pid", os.Getpid())

	if _, _, err := kernel.CheckVersion(minKernelVersion, true); err != nil {
		return 0, fmt.Errorf("check kernel version: %w", err)
	}

	c.global = new(global.Global)

	if c.flags.pprof != "" {
		f, err := os.Create(c.flags.pprof)
		if err != nil {
			return 0, fmt.Errorf("create pprof file: %w", err)
		}
		defer f.Close()

		if err := pprof.StartCPUProfile(f); err != nil {
			return 0, fmt.Errorf("start cpu profile: %w", err)
		}
		defer pprof.StopCPUProfile()
	}

	c.global.Config = config.New()
	if c.flags.config != "" {
		if err := c.global.Config.Load(c.flags.config); err != nil {
			return 1, fmt.Errorf("load config: %w", err)
		}
	}

	if err := socket.Init(); err != nil {
		return 1, fmt.Errorf("init socket: %w", err)
	}

	if os.Getenv("SUBTRACE_TOKEN") != "" && os.Getenv("SUBTRACE_LINK_ID_OVERRIDE") != "" {
		slog.Debug("SUBTRACE_LINK_ID_OVERRIDE is ignored when SUBTRACE_TOKEN is set")
	}

	if os.Getenv("SUBTRACE_TOKEN") != "" || c.flags.devtools == "" {
		go tracer.DefaultPublisher.Loop(ctx)
		defer func() {
			// TODO: should this be a different timeout value? or maybe wait forever
			// until some kind of forced user cancel (e.g. ctrl+c)? a dumb and simple
			// one second timeout is a good place to start.
			if flushed := tracer.DefaultPublisher.Flush(time.Second); !flushed {
				slog.Warn("subtrace might be exiting with unflushed data remaining in buffer")
			}
		}()
	}

	go stats.Loop(ctx)

	go c.watchSignals()

	if c.flags.log == nil {
		c.flags.log = new(bool)
		if os.Getenv("SUBTRACE_TOKEN") == "" {
			*c.flags.log = true
		} else {
			*c.flags.log = false
		}
	} else if *c.flags.log == false && os.Getenv("SUBTRACE_TOKEN") == "" {
		exists := false
		for _, arg := range os.Args {
			if strings.Contains(arg, "-log") {
				exists = true
				break
			}
			if arg == "--" {
				break
			}
		}

		if exists {
			slog.Warn("subtrace was started with -log=false but SUBTRACE_TOKEN is empty")
		}
	}

	tracer.DefaultManager.SetLog(*c.flags.log)

	go tracer.DefaultManager.StartBackgroundFlush(ctx)
	defer func() {
		if err := tracer.DefaultManager.Flush(); err != nil {
			slog.Error("failed to flush tracer event manager", "err", err)
		}
	}()

	if tls.Enabled {
		if err := tls.GenerateEphemeralCA(); err != nil {
			return 0, fmt.Errorf("create ephemeral TLS CA: %w", err)
		}
	}

	pid, sec, err := c.forkChild()
	if errors.Is(err, errMissingSysPtrace) {
		fmt.Fprintf(os.Stderr, "error: subtrace: missing SYS_PTRACE capability\n")
		fmt.Fprintf(os.Stderr, "\n")
		fmt.Fprintf(os.Stderr, "If you're using Docker, please add the --cap-add=SYS_PTRACE flag to\n")
		fmt.Fprintf(os.Stderr, "your `docker run` command when you start the container to fix this.\n")
		fmt.Fprintf(os.Stderr, "\n")
		fmt.Fprintf(os.Stderr, "See https://docs.subtrace.dev/ptrace for more details.\n")
		return 1, nil
	} else if err != nil {
		return 0, fmt.Errorf("exec child: %w", err)
	} else if sec == nil {
		return 127, nil
	}

	if c.flags.devtools != "" && !strings.HasPrefix(c.flags.devtools, "/") {
		c.flags.devtools = "/" + c.flags.devtools
	}
	c.global.Devtools = devtools.NewServer(c.flags.devtools)

	itab := socket.NewInodeTable()

	root, err := process.New(c.global, itab, pid)
	if err != nil {
		return 0, fmt.Errorf("new process: %w", err)
	}

	eng := engine.New(c.global, sec, itab, root)
	go eng.Start()

	log.SetLevel(log.Silent)

	var status unix.WaitStatus
	if _, err := unix.Wait4(pid, &status, 0, nil); err != nil {
		return 0, fmt.Errorf("wait4: %w", err)
	}
	slog.Debug("root process exited", "status", status.ExitStatus())

	eng.Wait()

	if err := eng.Close(); err != nil {
		slog.Debug("failed to close engine cleanly", "err", err) // not fatal
	}
	return status.ExitStatus(), nil
}

func (c *Command) watchSignals() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, unix.SIGINT, unix.SIGTERM, unix.SIGQUIT)
	for code := range ch {
		slog.Debug("tracer received signal", "code", code.String())
	}
}

var errMissingSysPtrace = fmt.Errorf("missing SYS_PTRACE")

// forkChild forks and re-executes the subtrace binary to run in child mode. It
// returns the child PID and the installed seccomp_unotify listener.
func (c *Command) forkChild() (pid int, sec *seccomp.Listener, err error) {
	memfd, err := unix.MemfdCreate("subtrace_seccomp_sync", unix.MFD_CLOEXEC)
	if err != nil {
		return 0, nil, fmt.Errorf("memfd_create: %w", err)
	}
	defer unix.Close(memfd)

	if err := unix.Ftruncate(memfd, 4); err != nil {
		return 0, nil, fmt.Errorf("ftruncate: %w", err)
	}

	addr, _, errno := unix.Syscall6(unix.SYS_MMAP, 0, 4, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED, uintptr(memfd), 0)
	if errno != 0 {
		return 0, nil, fmt.Errorf("mmap: %w", errno)
	}
	defer unix.Syscall6(unix.SYS_MUNMAP, addr, 4, 0, 0, 0, 0)
	*(*uint32)(unsafe.Pointer(addr)) = 0

	self, err := os.Executable()
	if err != nil {
		return 0, nil, fmt.Errorf("get executable: %w", err)
	}

	outfd := uintptr(1)
	errfd := uintptr(2)

	if journal.Enabled {
		mout, sout, err := createPTY()
		if err != nil {
			return 0, nil, fmt.Errorf("stdout pty: %w", err)
		}

		merr, serr, err := createPTY()
		if err != nil {
			return 0, nil, fmt.Errorf("stderr pty: %w", err)
		}

		c.global.Journal = journal.New()

		outfd = sout.Fd()
		errfd = serr.Fd()

		go func() {
			for {
				io.Copy(io.MultiWriter(os.Stdout, c.global.Journal.Stdout), mout)
			}
		}()

		go func() {
			for {
				io.Copy(io.MultiWriter(os.Stderr, c.global.Journal.Stderr), merr)
			}
		}()
	}

	pid, err = syscall.ForkExec(self, os.Args, &syscall.ProcAttr{
		Env:   append(os.Environ(), "_SUBTRACE_CHILD=true"),
		Files: []uintptr{0, outfd, errfd, uintptr(memfd)},
	})
	if err != nil {
		return 0, nil, fmt.Errorf("fork and exec: %w", err)
	}

	slog.Debug("waiting for child to install seccomp filter")
	start := time.Now()

	futex.Wait(unsafe.Pointer(addr), 0)
	wait := time.Since(start)

	secfd := atomic.LoadUint32((*uint32)(unsafe.Pointer(addr)))
	if secfd == ^uint32(0) {
		return 0, nil, nil
	}

	pidfd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return 0, nil, fmt.Errorf("pidfd_open: %w", err)
	}
	defer unix.Close(pidfd)

	ret, err := unix.PidfdGetfd(pidfd, int(secfd), 0)
	if err != nil {
		var errno syscall.Errno
		if errors.As(err, &errno) && errno == unix.EPERM {
			return 0, nil, fmt.Errorf("pidfd_getfd: %w: %w", errno, errMissingSysPtrace)
		}
		return 0, nil, fmt.Errorf("pidfd_getfd: %w (pidfd=%d, secfd=%d)", err, pidfd, secfd)
	}
	seccompfd := fd.NewFD(ret)
	defer seccompfd.DecRef()

	slog.Debug("initialized child", "pid", pid, "seccompfd", ret, slog.Group("took", "wait", wait.Nanoseconds(), "total", time.Since(start).Nanoseconds()))
	return pid, seccomp.NewFromFD(seccompfd), nil
}

func (c *Command) entrypointChild(ctx context.Context, args []string) error {
	addr, _, errno := unix.Syscall6(unix.SYS_MMAP, 0, 4, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED, uintptr(3), 0)
	if errno != 0 {
		return fmt.Errorf("mmap shared uint32: %w", errno)
	}

	abspath, err := exec.LookPath(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "subtrace: %s: command not found\n", args[0])
		atomic.StoreUint32((*uint32)(unsafe.Pointer(addr)), ^uint32(0))
		futex.Wake(unsafe.Pointer(addr), 1)
		os.Exit(127)
		return nil
	}

	environ := append(os.Environ(), "SUBTRACE_RUN=1")
	if tls.Enabled {
		environ = append(environ, tls.Environ()...)
	}

	var syscalls []int
	for nr, handler := range process.Handlers {
		if handler != nil {
			syscalls = append(syscalls, nr)
		}
	}

	fd, err := seccomp.InstallFilter(syscalls)
	if err != nil {
		atomic.StoreUint32((*uint32)(unsafe.Pointer(addr)), ^uint32(0))
		futex.Wake(unsafe.Pointer(addr), 1)
		return fmt.Errorf("create seccomp listener: %w", err)
	}
	slog.Debug("child: installed seccomp filter", "fd", fd)

	atomic.StoreUint32((*uint32)(unsafe.Pointer(addr)), uint32(fd))
	woke := futex.Wake(unsafe.Pointer(addr), 1)
	slog.Debug("child: notified parent", "woke", woke)

	unix.Syscall6(unix.SYS_MUNMAP, addr, 4, 0, 0, 0, 0)
	unix.Close(3)
	unix.Close(fd)

	slog.Debug("child: calling execve", "argv0", args[0], "abspath", abspath)
	if err := unix.Exec(abspath, args, environ); err != nil {
		return fmt.Errorf("execve: %w", err)
	}
	panic("unreachable")
}

func createPTY() (master, slave *os.File, err error) {
	master, err = os.Open("/dev/ptmx")
	if err != nil {
		return nil, nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}

	var name int
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, master.Fd(), unix.TIOCGPTN, uintptr(unsafe.Pointer(&name)))
	if errno != 0 {
		return nil, nil, fmt.Errorf("get pts name: %w", errno)
	}

	var unlock int // A value of zero corresponds to unlocking the pts
	_, _, errno = unix.Syscall(unix.SYS_IOCTL, master.Fd(), unix.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock)))
	if errno != 0 {
		return nil, nil, fmt.Errorf("unlock pts: %w", errno)
	}

	slave, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", name), unix.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open /dev/pts/%d: %w", name, err)
	}

	return master, slave, nil
}
