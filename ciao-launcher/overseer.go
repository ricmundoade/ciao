/*
// Copyright (c) 2016 Intel Corporation
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
*/

package main

import (
	"bufio"
	"container/list"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/golang/glog"

	"gopkg.in/yaml.v2"

	"github.com/01org/ciao/payloads"
	"github.com/01org/ciao/ssntp"
)

type ovsAddResult struct {
	cmdCh  chan<- interface{}
	canAdd bool
}

type ovsAddCmd struct {
	instance string
	cfg      *vmConfig
	targetCh chan<- ovsAddResult
}

type ovsGetResult struct {
	cmdCh   chan<- interface{}
	running ovsRunningState
}

type ovsGetCmd struct {
	instance string
	targetCh chan<- ovsGetResult
}

type ovsRemoveCmd struct {
	instance string
	suicide  bool
	errCh    chan<- error
}

type ovsStateChange struct {
	instance string
	state    ovsRunningState
}

type ovsStatsUpdateCmd struct {
	instance      string
	memoryUsageMB int
	diskUsageMB   int
	CPUUsage      int
}

type ovsTraceFrame struct {
	frame *ssntp.Frame
}

type ovsStatusCmd struct{}
type ovsStatsStatusCmd struct{}

type ovsRunningState int

const (
	ovsPending ovsRunningState = iota
	ovsRunning
	ovsStopped
)

const (
	diskSpaceHWM = 80 * 1000
	memHWM       = 1 * 1000
	diskSpaceLWM = 40 * 1000
	memLWM       = 512
)

type ovsInstanceState struct {
	cmdCh          chan<- interface{}
	running        ovsRunningState
	memoryUsageMB  int
	diskUsageMB    int
	CPUUsage       int
	maxDiskUsageMB int
	maxVCPUs       int
	maxMemoryMB    int
	sshIP          string
	sshPort        int
}

type overseer struct {
	instances          map[string]*ovsInstanceState
	ovsCh              chan interface{}
	childDoneCh        chan struct{}
	parentWg           *sync.WaitGroup
	childWg            *sync.WaitGroup
	ac                 *agentClient
	vcpusAllocated     int
	diskSpaceAllocated int
	memoryAllocated    int
	diskSpaceAvailable int
	memoryAvailable    int
	traceFrames        *list.List
}

type cnStats struct {
	totalMemMB      int
	availableMemMB  int
	totalDiskMB     int
	availableDiskMB int
	load            int
	cpusOnline      int
}

var memTotalRegexp *regexp.Regexp
var memFreeRegexp *regexp.Regexp
var memActiveFileRegexp *regexp.Regexp
var memInactiveFileRegexp *regexp.Regexp
var cpuStatsRegexp *regexp.Regexp

func init() {
	memTotalRegexp = regexp.MustCompile(`MemTotal:\s+(\d+)`)
	memFreeRegexp = regexp.MustCompile(`MemFree:\s+(\d+)`)
	memActiveFileRegexp = regexp.MustCompile(`Active\(file\):\s+(\d+)`)
	memInactiveFileRegexp = regexp.MustCompile(`Inactive\(file\):\s+(\d+)`)
	cpuStatsRegexp = regexp.MustCompile(`^cpu[0-9]+.*$`)
}

func grabInt(re *regexp.Regexp, line string, val *int) bool {
	matches := re.FindStringSubmatch(line)
	if matches != nil {
		parsedNum, err := strconv.Atoi(matches[1])
		if err == nil {
			*val = parsedNum
			return true
		}
	}
	return false
}

func getMemoryInfo() (total, available int) {

	total = -1
	available = -1
	free := -1
	active := -1
	inactive := -1

	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return
	}
	defer func() {
		_ = file.Close()
	}()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() && (total == -1 || free == -1 || active == -1 ||
		inactive == -1) {
		line := scanner.Text()
		for _, i := range []struct {
			v *int
			r *regexp.Regexp
		}{
			{&free, memFreeRegexp},
			{&total, memTotalRegexp},
			{&active, memActiveFileRegexp},
			{&inactive, memInactiveFileRegexp},
		} {
			if *i.v == -1 {
				if grabInt(i.r, line, i.v) {
					break
				}
			}
		}
	}

	if free != -1 && active != -1 && inactive != -1 {
		available = (free + active + inactive) / 1024
	}

	if total != -1 {
		total = total / 1024
	}

	return
}

func getOnlineCPUs() int {

	file, err := os.Open("/proc/stat")
	if err != nil {
		return -1
	}
	defer func() {
		_ = file.Close()
	}()

	scanner := bufio.NewScanner(file)
	if !scanner.Scan() {
		return -1
	}

	cpusOnline := 0
	for scanner.Scan() && cpuStatsRegexp.MatchString(scanner.Text()) {
		cpusOnline++
	}

	if cpusOnline == 0 {
		return -1
	}

	return cpusOnline
}

func getFSInfo() (total, available int) {

	total = -1
	available = -1
	var buf syscall.Statfs_t

	if syscall.Statfs(instancesDir, &buf) != nil {
		return
	}

	if buf.Bsize <= 0 {
		return
	}

	total = int((uint64(buf.Bsize) * buf.Blocks) / (1000 * 1000))
	available = int((uint64(buf.Bsize) * buf.Bavail) / (1000 * 1000))

	return
}

func getLoadAvg() int {
	file, err := os.Open("/proc/loadavg")
	if err != nil {
		return -1
	}
	defer func() {
		_ = file.Close()
	}()

	scanner := bufio.NewScanner(file)
	scanner.Split(bufio.ScanWords)
	if !scanner.Scan() {
		return -1
	}

	loadFloat, err := strconv.ParseFloat(scanner.Text(), 64)
	if err != nil {
		return -1
	}

	return int(loadFloat)
}

func (ovs *overseer) roomAvailable(cfg *vmConfig) bool {

	if len(ovs.instances) >= maxInstances {
		glog.Warningf("We're FULL.  Too many instances %d", len(ovs.instances))
		return false
	}

	diskSpaceAvailable := ovs.diskSpaceAvailable - cfg.Disk
	memoryAvailable := ovs.memoryAvailable - cfg.Mem

	glog.Infof("disk Avail %d MemAvail %d", diskSpaceAvailable, memoryAvailable)

	if diskSpaceAvailable < diskSpaceLWM {
		if diskLimit == true {
			return false
		}
	}

	if memoryAvailable < memLWM {
		if memLimit == true {
			return false
		}
	}

	return true
}

func (ovs *overseer) updateAvailableResources(cns *cnStats) {
	diskSpaceConsumed := 0
	memConsumed := 0
	for _, target := range ovs.instances {
		if target.diskUsageMB != -1 {
			diskSpaceConsumed += target.diskUsageMB
		}

		if target.memoryUsageMB != -1 {
			if target.memoryUsageMB < target.maxMemoryMB {
				memConsumed += target.memoryUsageMB
			} else {
				memConsumed += target.maxMemoryMB
			}
		}
	}

	ovs.diskSpaceAvailable = (cns.availableDiskMB + diskSpaceConsumed) -
		ovs.diskSpaceAllocated

	ovs.memoryAvailable = (cns.availableMemMB + memConsumed) -
		ovs.memoryAllocated

	if glog.V(1) {
		glog.Infof("Memory Available: %d Disk space Available %d",
			ovs.memoryAvailable, ovs.diskSpaceAvailable)
	}
}

func (ovs *overseer) computeStatus() ssntp.Status {

	if len(ovs.instances) >= maxInstances {
		return ssntp.FULL
	}

	if ovs.diskSpaceAvailable < diskSpaceHWM {
		if diskLimit == true {
			return ssntp.FULL
		}
	}

	if ovs.memoryAvailable < memHWM {
		if memLimit == true {
			return ssntp.FULL
		}
	}

	return ssntp.READY
}

func (ovs *overseer) sendStatusCommand(cns *cnStats, status ssntp.Status) {
	var s payloads.Ready

	s.Init()

	s.NodeUUID = ovs.ac.ssntpConn.UUID()
	s.MemTotalMB, s.MemAvailableMB = cns.totalMemMB, cns.availableMemMB
	s.Load = cns.load
	s.CpusOnline = cns.cpusOnline
	s.DiskTotalMB, s.DiskAvailableMB = cns.totalDiskMB, cns.availableDiskMB

	payload, err := yaml.Marshal(&s)
	if err != nil {
		glog.Errorf("Unable to Marshall Status %v", err)
		return
	}

	_, err = ovs.ac.ssntpConn.SendStatus(status, payload)
	if err != nil {
		glog.Errorf("Failed to send status command %v", err)
		return
	}
}

func (ovs *overseer) sendStats(cns *cnStats, status ssntp.Status) {
	var s payloads.Stat

	s.Init()

	s.NodeUUID = ovs.ac.ssntpConn.UUID()
	s.Status = status.String()
	s.MemTotalMB, s.MemAvailableMB = cns.totalMemMB, cns.availableMemMB
	s.Load = cns.load
	s.CpusOnline = cns.cpusOnline
	s.DiskTotalMB, s.DiskAvailableMB = cns.totalDiskMB, cns.availableDiskMB
	s.NodeHostName = hostname // global from network.go
	s.Networks = make([]payloads.NetworkStat, len(nicInfo))
	for i, nic := range nicInfo {
		s.Networks[i] = *nic
	}
	s.Instances = make([]payloads.InstanceStat, len(ovs.instances))
	i := 0
	for uuid, state := range ovs.instances {
		s.Instances[i].InstanceUUID = uuid
		if state.running == ovsRunning {
			s.Instances[i].State = payloads.Running
		} else if state.running == ovsStopped {
			s.Instances[i].State = payloads.Exited
		} else {
			s.Instances[i].State = payloads.Pending
		}
		s.Instances[i].MemoryUsageMB = state.memoryUsageMB
		s.Instances[i].DiskUsageMB = state.diskUsageMB
		s.Instances[i].CPUUsage = state.CPUUsage
		s.Instances[i].SSHIP = state.sshIP
		s.Instances[i].SSHPort = state.sshPort
		i++
	}

	payload, err := yaml.Marshal(&s)
	if err != nil {
		glog.Errorf("Unable to Marshall STATS %v", err)
		return
	}

	_, err = ovs.ac.ssntpConn.SendCommand(ssntp.STATS, payload)
	if err != nil {
		glog.Errorf("Failed to send stats command %v", err)
		return
	}
}

func (ovs *overseer) sendTraceReport() {
	var s payloads.Trace

	if ovs.traceFrames.Len() == 0 {
		return
	}

	for e := ovs.traceFrames.Front(); e != nil; e = e.Next() {
		f := e.Value.(*ssntp.Frame)
		frameTrace, err := f.DumpTrace()
		if err != nil {
			glog.Errorf("Unable to dump traced frame %v", err)
			continue
		}

		s.Frames = append(s.Frames, *frameTrace)
	}

	ovs.traceFrames = list.New()

	payload, err := yaml.Marshal(&s)
	if err != nil {
		glog.Errorf("Unable to Marshall TraceReport %v", err)
		return
	}

	_, err = ovs.ac.ssntpConn.SendEvent(ssntp.TraceReport, payload)
	if err != nil {
		glog.Errorf("Failed to send TraceReport event %v", err)
		return
	}
}

func getStats() *cnStats {
	var s cnStats

	s.totalMemMB, s.availableMemMB = getMemoryInfo()
	s.load = getLoadAvg()
	s.cpusOnline = getOnlineCPUs()
	s.totalDiskMB, s.availableDiskMB = getFSInfo()

	return &s
}

func (ovs *overseer) sendInstanceDeletedEvent(instance string) {
	var event payloads.EventInstanceDeleted

	event.InstanceDeleted.InstanceUUID = instance

	payload, err := yaml.Marshal(&event)
	if err != nil {
		glog.Errorf("Unable to Marshall STATS %v", err)
		return
	}

	_, err = ovs.ac.ssntpConn.SendEvent(ssntp.InstanceDeleted, payload)
	if err != nil {
		glog.Errorf("Failed to send event command %v", err)
		return
	}
}

func (ovs *overseer) processCommand(cmd interface{}) {
	switch cmd := cmd.(type) {
	case *ovsGetCmd:
		glog.Infof("Overseer: looking for instance %s", cmd.instance)
		var insState ovsGetResult
		target := ovs.instances[cmd.instance]
		if target != nil {
			insState.cmdCh = target.cmdCh
			insState.running = target.running
		}
		cmd.targetCh <- insState
	case *ovsAddCmd:
		glog.Infof("Overseer: adding %s", cmd.instance)
		var targetCh chan<- interface{}
		target := ovs.instances[cmd.instance]
		canAdd := true
		cfg := cmd.cfg
		if target != nil {
			targetCh = target.cmdCh
		} else if ovs.roomAvailable(cfg) {
			ovs.vcpusAllocated += cfg.Cpus
			ovs.diskSpaceAllocated += cfg.Disk
			ovs.memoryAllocated += cfg.Mem
			targetCh = startInstance(cmd.instance, cfg, ovs.childWg, ovs.childDoneCh,
				ovs.ac, ovs.ovsCh)
			ovs.instances[cmd.instance] = &ovsInstanceState{
				cmdCh:          targetCh,
				running:        ovsPending,
				diskUsageMB:    -1,
				CPUUsage:       -1,
				memoryUsageMB:  -1,
				maxDiskUsageMB: cfg.Disk,
				maxVCPUs:       cfg.Cpus,
				maxMemoryMB:    cfg.Mem,
				sshIP:          cfg.ConcIP,
				sshPort:        cfg.SSHPort,
			}
		} else {
			canAdd = false
		}
		cmd.targetCh <- ovsAddResult{targetCh, canAdd}
	case *ovsRemoveCmd:
		glog.Infof("Overseer: removing %s", cmd.instance)
		target := ovs.instances[cmd.instance]
		if target == nil {
			cmd.errCh <- fmt.Errorf("Instance does not exist")
			break
		}

		ovs.diskSpaceAllocated -= target.maxDiskUsageMB
		if ovs.diskSpaceAllocated < 0 {
			ovs.diskSpaceAllocated = 0
		}

		ovs.vcpusAllocated -= target.maxVCPUs
		if ovs.vcpusAllocated < 0 {
			ovs.vcpusAllocated = 0
		}

		ovs.memoryAllocated -= target.maxMemoryMB
		if ovs.memoryAllocated < 0 {
			ovs.memoryAllocated = 0
		}

		delete(ovs.instances, cmd.instance)
		if !cmd.suicide {
			ovs.sendInstanceDeletedEvent(cmd.instance)
		}
		cmd.errCh <- nil
	case *ovsStatusCmd:
		glog.Info("Overseer: Recieved Status Command")
		if !ovs.ac.ssntpConn.isConnected() {
			break
		}
		cns := getStats()
		ovs.updateAvailableResources(cns)
		ovs.sendStatusCommand(cns, ovs.computeStatus())
	case *ovsStatsStatusCmd:
		glog.Info("Overseer: Recieved StatsStatus Command")
		if !ovs.ac.ssntpConn.isConnected() {
			break
		}
		cns := getStats()
		ovs.updateAvailableResources(cns)
		status := ovs.computeStatus()
		ovs.sendStatusCommand(cns, status)
		ovs.sendStats(cns, status)
	case *ovsStateChange:
		glog.Infof("Overseer: Recieved State Change %v", *cmd)
		target := ovs.instances[cmd.instance]
		if target != nil {
			target.running = cmd.state
		}
	case *ovsStatsUpdateCmd:
		if glog.V(1) {
			glog.Infof("STATS Update for %s: Mem %d Disk %d Cpu %d",
				cmd.instance, cmd.memoryUsageMB,
				cmd.diskUsageMB, cmd.CPUUsage)
		}
		target := ovs.instances[cmd.instance]
		if target != nil {
			target.memoryUsageMB = cmd.memoryUsageMB
			target.diskUsageMB = cmd.diskUsageMB
			target.CPUUsage = cmd.CPUUsage
		}
	case *ovsTraceFrame:
		cmd.frame.SetEndStamp()
		ovs.traceFrames.PushBack(cmd.frame)
	default:
		panic("Unknown Overseer Command")
	}
}

func (ovs *overseer) runOverseer() {

	statsTimer := time.After(time.Second * statsPeriod)
DONE:
	for {
		select {
		case cmd, ok := <-ovs.ovsCh:
			if !ok {
				break DONE
			}
			ovs.processCommand(cmd)
		case <-statsTimer:
			if !ovs.ac.ssntpConn.isConnected() {
				statsTimer = time.After(time.Second * statsPeriod)
				continue
			}

			cns := getStats()
			ovs.updateAvailableResources(cns)
			status := ovs.computeStatus()
			ovs.sendStatusCommand(cns, status)
			ovs.sendStats(cns, status)
			ovs.sendTraceReport()
			statsTimer = time.After(time.Second * statsPeriod)
			if glog.V(1) {
				glog.Infof("Consumed: Disk %d Mem %d CPUs %d",
					ovs.diskSpaceAllocated, ovs.memoryAllocated, ovs.vcpusAllocated)
			}
		}
	}

	close(ovs.childDoneCh)
	ovs.childWg.Wait()
	glog.Info("All instance go routines have exitted")
	ovs.parentWg.Done()

	glog.Info("Overseer exitting")
}

func startOverseer(wg *sync.WaitGroup, ac *agentClient) chan<- interface{} {

	instances := make(map[string]*ovsInstanceState)
	ovsCh := make(chan interface{})
	toMonitor := make([]chan<- interface{}, 0, 1024)
	childDoneCh := make(chan struct{})
	childWg := new(sync.WaitGroup)

	vcpusAllocated := 0
	diskSpaceAllocated := 0
	memoryAllocated := 0

	_ = filepath.Walk(instancesDir, func(path string, info os.FileInfo, err error) error {
		if path == instancesDir {
			return nil
		}

		if !info.IsDir() {
			return nil
		}

		glog.Infof("Reconnecting to existing instance %s", path)
		instance := filepath.Base(path)

		// BUG(markus): We should garbage collect corrupt instances

		cfg, err := loadVMConfig(path)
		if err != nil {
			glog.Warning("Unable to load state of running instance %s: %v", instance, err)
			return nil
		}

		vcpusAllocated += cfg.Cpus
		diskSpaceAllocated += cfg.Disk
		memoryAllocated += cfg.Mem

		target := startInstance(instance, cfg, childWg, childDoneCh, ac, ovsCh)
		instances[instance] = &ovsInstanceState{
			cmdCh:          target,
			running:        ovsPending,
			diskUsageMB:    -1,
			CPUUsage:       -1,
			memoryUsageMB:  -1,
			maxDiskUsageMB: cfg.Disk,
			maxVCPUs:       cfg.Cpus,
			maxMemoryMB:    cfg.Mem,
			sshIP:          cfg.ConcIP,
			sshPort:        cfg.SSHPort,
		}
		toMonitor = append(toMonitor, target)

		return filepath.SkipDir
	})

	ovs := &overseer{
		instances:          instances,
		ovsCh:              ovsCh,
		parentWg:           wg,
		childWg:            childWg,
		childDoneCh:        childDoneCh,
		ac:                 ac,
		vcpusAllocated:     vcpusAllocated,
		diskSpaceAllocated: diskSpaceAllocated,
		memoryAllocated:    memoryAllocated,
		traceFrames:        list.New(),
	}
	ovs.parentWg.Add(1)
	glog.Info("Starting Overseer")
	glog.Infof("Allocated: Disk %d Mem %d CPUs %d",
		diskSpaceAllocated, memoryAllocated, vcpusAllocated)
	go ovs.runOverseer()
	ovs = nil
	instances = nil

	// I know this looks weird but there is method here.  After we launch the overseer go routine
	// we can no longer access instances from this go routine otherwise we will have a data race.
	// For this reason we make a copy of the instance command channels that can be safely used
	// in this go routine.  The monitor commands cannot be sent from the overseer as it is not
	// allowed to send information to the instance go routines.  Doing so would incur the risk of
	// deadlock.  So we copy.  'A little copying is better than a little dependency', and so forth.

	for _, v := range toMonitor {
		v <- &insMonitorCmd{}
	}

	return ovsCh
}
