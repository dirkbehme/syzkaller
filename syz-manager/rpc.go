// Copyright 2018 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package main

import (
	"bytes"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/syzkaller/pkg/cover"
	"github.com/google/syzkaller/pkg/fuzzer"
	"github.com/google/syzkaller/pkg/host"
	"github.com/google/syzkaller/pkg/ipc"
	"github.com/google/syzkaller/pkg/log"
	"github.com/google/syzkaller/pkg/mgrconfig"
	"github.com/google/syzkaller/pkg/rpctype"
	"github.com/google/syzkaller/pkg/signal"
	"github.com/google/syzkaller/pkg/stats"
	"github.com/google/syzkaller/pkg/vminfo"
	"github.com/google/syzkaller/prog"
)

type RPCServer struct {
	mgr     RPCManagerView
	cfg     *mgrconfig.Config
	target  *prog.Target
	server  *rpctype.RPCServer
	checker *vminfo.Checker
	port    int

	checkDone             bool
	checkFailures         int
	checkFeatures         *host.Features
	targetEnabledSyscalls map[*prog.Syscall]bool
	canonicalModules      *cover.Canonicalizer

	mu      sync.Mutex
	runners sync.Map // Instead of map[string]*Runner.

	// We did not finish these requests because of VM restarts.
	// They will be eventually given to other VMs.
	rescuedInputs []*fuzzer.Request

	statExecs                 *stats.Val
	statExecRetries           *stats.Val
	statExecutorRestarts      *stats.Val
	statExecBufferTooSmall    *stats.Val
	statVMRestarts            *stats.Val
	statExchangeCalls         *stats.Val
	statExchangeProgs         *stats.Val
	statExchangeServerLatency *stats.Val
	statExchangeClientLatency *stats.Val
}

type Runner struct {
	name       string
	injectLog  chan<- []byte
	injectStop chan bool

	machineInfo []byte
	instModules *cover.CanonicalizerInstance

	// The mutex protects newMaxSignal, dropMaxSignal, and requests.
	mu            sync.Mutex
	newMaxSignal  signal.Signal
	dropMaxSignal signal.Signal
	nextRequestID atomic.Int64
	requests      map[int64]Request
}

type Request struct {
	req    *fuzzer.Request
	try    int
	procID int
}

type BugFrames struct {
	memoryLeaks []string
	dataRaces   []string
}

// RPCManagerView restricts interface between RPCServer and Manager.
type RPCManagerView interface {
	fuzzerConnect() (BugFrames, map[uint32]uint32, signal.Signal)
	machineChecked(features *host.Features, globFiles map[string][]string,
		enabledSyscalls map[*prog.Syscall]bool, modules []host.KernelModule)
	getFuzzer() *fuzzer.Fuzzer
}

func startRPCServer(mgr *Manager) (*RPCServer, error) {
	serv := &RPCServer{
		mgr:       mgr,
		cfg:       mgr.cfg,
		target:    mgr.target,
		checker:   vminfo.New(mgr.cfg),
		statExecs: mgr.statExecs,
		statExecRetries: stats.Create("exec retries",
			"Number of times a test program was restarted because the first run failed",
			stats.Rate{}, stats.Graph("executor")),
		statExecutorRestarts: stats.Create("executor restarts",
			"Number of times executor process was restarted", stats.Rate{}, stats.Graph("executor")),
		statExecBufferTooSmall: stats.Create("buffer too small",
			"Program serialization overflowed exec buffer", stats.NoGraph),
		statVMRestarts: stats.Create("vm restarts", "Total number of VM starts",
			stats.Rate{}, stats.NoGraph),
		statExchangeCalls: stats.Create("exchange calls", "Number of RPC Exchange calls",
			stats.Rate{}),
		statExchangeProgs: stats.Create("exchange progs", "Test programs exchanged per RPC call",
			stats.Distribution{}),
		statExchangeServerLatency: stats.Create("exchange manager latency",
			"Manager RPC Exchange call latency (us)", stats.Distribution{}),
		statExchangeClientLatency: stats.Create("exchange fuzzer latency",
			"End-to-end fuzzer RPC Exchange call latency (us)", stats.Distribution{}),
	}
	s, err := rpctype.NewRPCServer(mgr.cfg.RPC, "Manager", serv, mgr.netCompression)
	if err != nil {
		return nil, err
	}
	log.Logf(0, "serving rpc on tcp://%v", s.Addr())
	serv.port = s.Addr().(*net.TCPAddr).Port
	serv.server = s
	go s.Serve()
	return serv, nil
}

func (serv *RPCServer) Connect(a *rpctype.ConnectArgs, r *rpctype.ConnectRes) error {
	log.Logf(1, "fuzzer %v connected", a.Name)
	serv.statVMRestarts.Add(1)

	serv.mu.Lock()
	defer serv.mu.Unlock()
	r.EnabledCalls = serv.cfg.Syscalls
	r.GitRevision = prog.GitRevision
	r.TargetRevision = serv.cfg.Target.Revision
	r.Features = serv.checkFeatures
	r.ReadFiles = serv.checker.RequiredFiles()
	if !serv.checkDone {
		r.ReadGlobs = serv.target.RequiredGlobs()
	}
	return nil
}

func (serv *RPCServer) Check(a *rpctype.CheckArgs, r *rpctype.CheckRes) error {
	serv.mu.Lock()
	defer serv.mu.Unlock()

	modules, machineInfo, err := serv.checker.MachineInfo(a.Files)
	if err != nil {
		log.Logf(0, "parsing of machine info failed: %v", err)
		if a.Error == "" {
			a.Error = err.Error()
		}
	}

	if !serv.checkDone {
		if err := serv.check(a, modules); err != nil {
			return err
		}
		serv.checkDone = true
	}

	bugFrames, execCoverFilter, maxSignal := serv.mgr.fuzzerConnect()

	runner := serv.findRunner(a.Name)
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if runner.machineInfo != nil {
		return fmt.Errorf("duplicate connection from %s", a.Name)
	}
	runner.machineInfo = machineInfo
	runner.instModules = serv.canonicalModules.NewInstance(modules)
	runner.newMaxSignal = maxSignal

	r.MemoryLeakFrames = bugFrames.memoryLeaks
	r.DataRaceFrames = bugFrames.dataRaces
	instCoverFilter := runner.instModules.DecanonicalizeFilter(execCoverFilter)
	r.CoverFilterBitmap = createCoverageBitmap(serv.cfg.SysTarget, instCoverFilter)
	return nil
}

func (serv *RPCServer) check(a *rpctype.CheckArgs, modules []host.KernelModule) error {
	// Note: need to print disbled syscalls before failing due to an error.
	// This helps to debug "all system calls are disabled".
	if len(serv.cfg.EnabledSyscalls) != 0 && len(a.DisabledCalls[serv.cfg.Sandbox]) != 0 {
		disabled := make(map[string]string)
		for _, dc := range a.DisabledCalls[serv.cfg.Sandbox] {
			disabled[serv.cfg.Target.Syscalls[dc.ID].Name] = dc.Reason
		}
		for _, id := range serv.cfg.Syscalls {
			name := serv.cfg.Target.Syscalls[id].Name
			if reason := disabled[name]; reason != "" {
				log.Logf(0, "disabling %v: %v", name, reason)
			}
		}
	}
	for _, file := range a.Files {
		if file.Error != "" {
			log.Logf(0, "failed to read %q: %v", file.Name, file.Error)
		}
	}
	if a.Error != "" {
		log.Logf(0, "machine check failed: %v", a.Error)
		serv.checkFailures++
		if serv.checkFailures == 10 {
			log.Fatalf("machine check failing")
		}
		return fmt.Errorf("machine check failed: %v", a.Error)
	}
	serv.targetEnabledSyscalls = make(map[*prog.Syscall]bool)
	for _, call := range a.EnabledCalls[serv.cfg.Sandbox] {
		serv.targetEnabledSyscalls[serv.cfg.Target.Syscalls[call]] = true
	}
	log.Logf(0, "machine check:")
	log.Logf(0, "%-24v: %v/%v", "syscalls", len(serv.targetEnabledSyscalls), len(serv.cfg.Target.Syscalls))
	for _, feat := range a.Features.Supported() {
		log.Logf(0, "%-24v: %v", feat.Name, feat.Reason)
	}
	serv.checkFeatures = a.Features
	serv.canonicalModules = cover.NewCanonicalizer(modules, serv.cfg.Cover)
	serv.mgr.machineChecked(a.Features, a.Globs, serv.targetEnabledSyscalls, modules)
	return nil
}

func (serv *RPCServer) StartExecuting(a *rpctype.ExecutingRequest, r *int) error {
	serv.statExecs.Add(1)
	if a.Try != 0 {
		serv.statExecRetries.Add(1)
	}
	runner := serv.findRunner(a.Name)
	if runner == nil {
		return nil
	}
	runner.mu.Lock()
	req, ok := runner.requests[a.ID]
	if !ok {
		runner.mu.Unlock()
		return nil
	}
	// RPC handlers are invoked in separate goroutines, so start executing notifications
	// can outrun each other and completion notification.
	if req.try < a.Try {
		req.try = a.Try
		req.procID = a.ProcID
	}
	runner.requests[a.ID] = req
	runner.mu.Unlock()
	runner.logProgram(a.ProcID, req.req.Prog)
	return nil
}

func (serv *RPCServer) ExchangeInfo(a *rpctype.ExchangeInfoRequest, r *rpctype.ExchangeInfoReply) error {
	start := time.Now()
	runner := serv.findRunner(a.Name)
	if runner == nil {
		return nil
	}

	fuzzerObj := serv.mgr.getFuzzer()
	if fuzzerObj == nil {
		// ExchangeInfo calls follow MachineCheck, so the fuzzer must have been initialized.
		panic("exchange info call with nil fuzzer")
	}

	appendRequest := func(inp *fuzzer.Request) {
		if req, ok := runner.newRequest(inp); ok {
			r.Requests = append(r.Requests, req)
		} else {
			// It's bad if we systematically fail to serialize programs,
			// but so far we don't have a better handling than counting this.
			// This error is observed a lot on the seeded syz_mount_image calls.
			serv.statExecBufferTooSmall.Add(1)
			fuzzerObj.Done(inp, &fuzzer.Result{Stop: true})
		}
	}

	// Try to collect some of the postponed requests.
	if serv.mu.TryLock() {
		for len(serv.rescuedInputs) != 0 && len(r.Requests) < a.NeedProgs {
			last := len(serv.rescuedInputs) - 1
			inp := serv.rescuedInputs[last]
			serv.rescuedInputs[last] = nil
			serv.rescuedInputs = serv.rescuedInputs[:last]
			appendRequest(inp)
		}
		serv.mu.Unlock()
	}

	// First query new inputs and only then post results.
	// It should foster a more even distribution of executions
	// across all VMs.
	for len(r.Requests) < a.NeedProgs {
		appendRequest(fuzzerObj.NextInput())
	}

	for _, result := range a.Results {
		serv.doneRequest(runner, result, fuzzerObj)
	}

	stats.Import(a.StatsDelta)

	runner.mu.Lock()
	// Let's transfer new max signal in portions.

	const transferMaxSignal = 500000
	newSignal := runner.newMaxSignal.Split(transferMaxSignal)
	dropSignal := runner.dropMaxSignal.Split(transferMaxSignal)
	runner.mu.Unlock()

	r.NewMaxSignal = runner.instModules.Decanonicalize(newSignal.ToRaw())
	r.DropMaxSignal = runner.instModules.Decanonicalize(dropSignal.ToRaw())

	log.Logf(2, "exchange with %s: %d done, %d new requests, %d new max signal, %d drop signal",
		a.Name, len(a.Results), len(r.Requests), len(r.NewMaxSignal), len(r.DropMaxSignal))

	serv.statExchangeCalls.Add(1)
	serv.statExchangeProgs.Add(a.NeedProgs)
	serv.statExchangeClientLatency.Add(int(a.Latency.Microseconds()))
	serv.statExchangeServerLatency.Add(int(time.Since(start).Microseconds()))
	return nil
}

func (serv *RPCServer) findRunner(name string) *Runner {
	if val, _ := serv.runners.Load(name); val != nil {
		return val.(*Runner)
	}
	// There might be a parallel shutdownInstance().
	// Ignore requests then.
	return nil
}

func (serv *RPCServer) createInstance(name string, injectLog chan<- []byte) {
	runner := &Runner{
		name:       name,
		requests:   make(map[int64]Request),
		injectLog:  injectLog,
		injectStop: make(chan bool),
	}
	if _, loaded := serv.runners.LoadOrStore(name, runner); loaded {
		panic(fmt.Sprintf("duplicate instance %s", name))
	}
}

func (serv *RPCServer) shutdownInstance(name string, crashed bool) []byte {
	runnerPtr, _ := serv.runners.LoadAndDelete(name)
	runner := runnerPtr.(*Runner)
	runner.mu.Lock()
	if runner.requests == nil {
		// We are supposed to invoke this code only once.
		panic("Runner.requests is already nil")
	}
	oldRequests := runner.requests
	runner.requests = nil
	runner.mu.Unlock()

	close(runner.injectStop)

	// The VM likely crashed, so let's tell pkg/fuzzer to abort the affected jobs.
	// fuzzerObj may be null, but in that case oldRequests would be empty as well.
	serv.mu.Lock()
	defer serv.mu.Unlock()
	fuzzerObj := serv.mgr.getFuzzer()
	for _, req := range oldRequests {
		if crashed && req.try >= 0 {
			fuzzerObj.Done(req.req, &fuzzer.Result{Stop: true})
		} else {
			// We will resend these inputs to another VM.
			serv.rescuedInputs = append(serv.rescuedInputs, req.req)
		}
	}
	return runner.machineInfo
}

func (serv *RPCServer) distributeSignalDelta(plus, minus signal.Signal) {
	serv.runners.Range(func(key, value any) bool {
		runner := value.(*Runner)
		runner.mu.Lock()
		defer runner.mu.Unlock()
		runner.newMaxSignal.Merge(plus)
		runner.dropMaxSignal.Merge(minus)
		return true
	})
}

func (serv *RPCServer) doneRequest(runner *Runner, resp rpctype.ExecutionResult, fuzzerObj *fuzzer.Fuzzer) {
	info := &resp.Info
	if info.Freshness == 0 {
		serv.statExecutorRestarts.Add(1)
	}
	runner.mu.Lock()
	req, ok := runner.requests[resp.ID]
	if ok {
		delete(runner.requests, resp.ID)
	}
	runner.mu.Unlock()
	if !ok {
		// There may be a concurrent shutdownInstance() call.
		return
	}
	// RPC handlers are invoked in separate goroutines, so log the program here
	// if completion notification outrun start executing notification.
	if req.try < resp.Try {
		runner.logProgram(resp.ProcID, req.req.Prog)
	}
	if !serv.cfg.Cover {
		addFallbackSignal(req.req.Prog, info)
	}
	for i := 0; i < len(info.Calls); i++ {
		call := &info.Calls[i]
		call.Cover = runner.instModules.Canonicalize(call.Cover)
		call.Signal = runner.instModules.Canonicalize(call.Signal)
	}
	info.Extra.Cover = runner.instModules.Canonicalize(info.Extra.Cover)
	info.Extra.Signal = runner.instModules.Canonicalize(info.Extra.Signal)
	fuzzerObj.Done(req.req, &fuzzer.Result{Info: info})
}

func (runner *Runner) newRequest(req *fuzzer.Request) (rpctype.ExecutionRequest, bool) {
	progData, err := req.Prog.SerializeForExec()
	if err != nil {
		return rpctype.ExecutionRequest{}, false
	}

	var signalFilter signal.Signal
	if req.SignalFilter != nil {
		newRawSignal := runner.instModules.Decanonicalize(req.SignalFilter.ToRaw())
		// We don't care about specific priorities here.
		signalFilter = signal.FromRaw(newRawSignal, 0)
	}
	id := runner.nextRequestID.Add(1)
	runner.mu.Lock()
	if runner.requests != nil {
		runner.requests[id] = Request{
			req: req,
			try: -1,
		}
	}
	runner.mu.Unlock()
	return rpctype.ExecutionRequest{
		ID:               id,
		ProgData:         progData,
		NeedCover:        req.NeedCover,
		NeedSignal:       req.NeedSignal,
		SignalFilter:     signalFilter,
		SignalFilterCall: req.SignalFilterCall,
		NeedHints:        req.NeedHints,
	}, true
}

func (runner *Runner) logProgram(procID int, p *prog.Prog) {
	buf := new(bytes.Buffer)
	fmt.Fprintf(buf, "executing program %v:\n%s\n", procID, p.Serialize())
	select {
	case runner.injectLog <- buf.Bytes():
	case <-runner.injectStop:
	}
}

// addFallbackSignal computes simple fallback signal in cases we don't have real coverage signal.
// We use syscall number or-ed with returned errno value as signal.
// At least this gives us all combinations of syscall+errno.
func addFallbackSignal(p *prog.Prog, info *ipc.ProgInfo) {
	callInfos := make([]prog.CallInfo, len(info.Calls))
	for i, inf := range info.Calls {
		if inf.Flags&ipc.CallExecuted != 0 {
			callInfos[i].Flags |= prog.CallExecuted
		}
		if inf.Flags&ipc.CallFinished != 0 {
			callInfos[i].Flags |= prog.CallFinished
		}
		if inf.Flags&ipc.CallBlocked != 0 {
			callInfos[i].Flags |= prog.CallBlocked
		}
		callInfos[i].Errno = inf.Errno
	}
	p.FallbackSignal(callInfos)
	for i, inf := range callInfos {
		info.Calls[i].Signal = inf.Signal
	}
}
