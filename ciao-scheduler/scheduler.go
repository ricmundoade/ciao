//
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
//

package main

import (
	"flag"
	"fmt"
	"github.com/01org/ciao/payloads"
	"github.com/01org/ciao/ssntp"
	"github.com/golang/glog"
	"gopkg.in/yaml.v2"
	"log"
	"os"
	"runtime/pprof"
	"sync"
	"syscall"
	"time"
)

type ssntpSchedulerServer struct {
	ssntp ssntp.Server
	name  string
	// Command & Status Reporting node(s)
	controllerMap   map[string]*controllerStat
	controllerMutex sync.RWMutex // Rlock traversal of map, Lock modification of map
	// Compute Nodes
	cnMap      map[string]*nodeStat
	cnList     []*nodeStat
	cnMutex    sync.RWMutex // Rlock traversal of map, Lock modification of map
	cnMRU      *nodeStat
	cnMRUIndex int
	//cnInactiveMap      map[string]nodeStat
	// Network Nodes
	nnMap   map[string]*nodeStat
	nnMutex sync.RWMutex // Rlock traversal of map, Lock modification of map
	nnMRU   string
}

func newSsntpSchedulerServer() *ssntpSchedulerServer {
	return &ssntpSchedulerServer{
		name:          "Ciao Scheduler Server",
		controllerMap: make(map[string]*controllerStat),
		cnMap:         make(map[string]*nodeStat),
		cnMRUIndex:    -1,
		nnMap:         make(map[string]*nodeStat),
	}
}

type nodeStat struct {
	mutex      sync.Mutex
	status     ssntp.Status
	uuid       string
	memTotalMB int
	memAvailMB int
	load       int
	cpus       int
}

type controllerStatus uint8

func (s controllerStatus) String() string {
	switch s {
	case controllerMaster:
		return "MASTER"
	case controllerBackup:
		return "BACKUP"
	}

	return ""
}

const (
	controllerMaster controllerStatus = iota
	controllerBackup
)

type controllerStat struct {
	mutex  sync.Mutex
	status controllerStatus
	uuid   string
}

func (sched *ssntpSchedulerServer) sendNodeConnectionEvent(nodeUUID, controllerUUID string, nodeType payloads.Resource, connected bool) (int, error) {
	/* connect */
	if connected == true {
		payload := payloads.NodeConnected{
			Connected: payloads.NodeConnectedEvent{
				NodeUUID: nodeUUID,
				NodeType: nodeType,
			},
		}

		b, err := yaml.Marshal(&payload)
		if err != nil {
			return 0, err
		}

		return sched.ssntp.SendEvent(controllerUUID, ssntp.NodeConnected, b)
	}

	/* disconnect */
	payload := payloads.NodeDisconnected{
		Disconnected: payloads.NodeConnectedEvent{
			NodeUUID: nodeUUID,
			NodeType: nodeType,
		},
	}

	b, err := yaml.Marshal(&payload)
	if err != nil {
		return 0, err
	}

	return sched.ssntp.SendEvent(controllerUUID, ssntp.NodeDisconnected, b)
}

func (sched *ssntpSchedulerServer) sendNodeConnectedEvents(nodeUUID string, nodeType payloads.Resource) {
	sched.controllerMutex.RLock()
	defer sched.controllerMutex.RUnlock()

	for _, c := range sched.controllerMap {
		sched.sendNodeConnectionEvent(nodeUUID, c.uuid, nodeType, true)
	}
}

func (sched *ssntpSchedulerServer) sendNodeDisconnectedEvents(nodeUUID string, nodeType payloads.Resource) {
	sched.controllerMutex.RLock()
	defer sched.controllerMutex.RUnlock()

	for _, c := range sched.controllerMap {
		sched.sendNodeConnectionEvent(nodeUUID, c.uuid, nodeType, false)
	}
}

// Add state for newly connected Controller
// This function is symmetric with disconnectController().
func (sched *ssntpSchedulerServer) connectController(uuid string) {
	sched.controllerMutex.Lock()
	defer sched.controllerMutex.Unlock()

	if sched.controllerMap[uuid] != nil {
		glog.Warningf("Unexpected reconnect from controller %s\n", uuid)
		return
	}

	var controller controllerStat

	// TODO: smarter clustering than "assume master, unless another is master"
	controller.status = controllerMaster
	for _, c := range sched.controllerMap {
		c.mutex.Lock()
		if c.status == controllerMaster {
			controller.status = controllerBackup
			c.mutex.Unlock()
			break
		}
		c.mutex.Unlock()
	}

	controller.uuid = uuid
	sched.controllerMap[uuid] = &controller
}

// Undo previous state additions for departed Controller
// This function is symmetric with connectController().
func (sched *ssntpSchedulerServer) disconnectController(uuid string) {
	sched.controllerMutex.Lock()
	defer sched.controllerMutex.Unlock()

	controller := sched.controllerMap[uuid]
	if controller == nil {
		glog.Warningf("Unexpected disconnect from controller %s\n", uuid)
		return
	}
	delete(sched.controllerMap, uuid)

	if controller.status == controllerBackup {
		return
	} // else promote a new master
	for _, c := range sched.controllerMap {
		c.mutex.Lock()
		if c.status == controllerBackup {
			c.status = controllerMaster
			//TODO: inform the Controller it is master
			c.mutex.Unlock()
			break
		}
		c.mutex.Unlock()
	}
}

// Add state for newly connected Compute Node
// This function is symmetric with disconnectComputeNode().
func (sched *ssntpSchedulerServer) connectComputeNode(uuid string) {
	sched.cnMutex.Lock()
	defer sched.cnMutex.Unlock()

	if sched.cnMap[uuid] != nil {
		glog.Warningf("Unexpected reconnect from compute node %s\n", uuid)
		return
	}

	var node nodeStat
	node.status = ssntp.CONNECTED
	node.uuid = uuid
	sched.cnList = append(sched.cnList, &node)
	sched.cnMap[uuid] = &node

	sched.sendNodeConnectedEvents(uuid, payloads.ComputeNode)
}

// Undo previous state additions for departed Compute Node
// This function is symmetric with connectComputeNode().
func (sched *ssntpSchedulerServer) disconnectComputeNode(uuid string) {
	sched.cnMutex.Lock()
	defer sched.cnMutex.Unlock()

	node := sched.cnMap[uuid]
	if node == nil {
		glog.Warningf("Unexpected disconnect from compute node %s\n", uuid)
		return
	}

	//TODO: consider moving to cnInactiveMap?
	delete(sched.cnMap, uuid)

	for i, n := range sched.cnList {
		if n != node {
			continue
		}

		sched.cnList = append(sched.cnList[:i], sched.cnList[i+1:]...)
	}

	if node == sched.cnMRU {
		sched.cnMRU = nil
		sched.cnMRUIndex = -1
	}

	sched.sendNodeDisconnectedEvents(uuid, payloads.ComputeNode)
}

// Add state for newly connected Network Node
// This function is symmetric with disconnectNetworkNode().
func (sched *ssntpSchedulerServer) connectNetworkNode(uuid string) {
	sched.nnMutex.Lock()
	defer sched.nnMutex.Unlock()

	if sched.nnMap[uuid] != nil {
		glog.Warningf("Unexpected reconnect from network compute node %s\n", uuid)
		return
	}

	var node nodeStat
	node.status = ssntp.CONNECTED
	node.uuid = uuid
	sched.nnMap[uuid] = &node

	sched.sendNodeConnectedEvents(uuid, payloads.NetworkNode)
}

// Undo previous state additions for departed Network Node
// This function is symmetric with connectNetworkNode().
func (sched *ssntpSchedulerServer) disconnectNetworkNode(uuid string) {
	sched.nnMutex.Lock()
	defer sched.nnMutex.Unlock()

	if sched.nnMap[uuid] == nil {
		glog.Warningf("Unexpected disconnect from network compute node %s\n", uuid)
		return
	}

	//TODO: consider moving to nnInactiveMap?
	delete(sched.nnMap, uuid)

	sched.sendNodeDisconnectedEvents(uuid, payloads.NetworkNode)
}
func (sched *ssntpSchedulerServer) ConnectNotify(uuid string, role uint32) {
	switch role {
	case ssntp.Controller:
		sched.connectController(uuid)
	case ssntp.AGENT:
		sched.connectComputeNode(uuid)
	case ssntp.NETAGENT:
		sched.connectNetworkNode(uuid)
	}

	glog.V(2).Infof("Connect (role 0x%x, uuid=%s)\n", role, uuid)
}

func (sched *ssntpSchedulerServer) DisconnectNotify(uuid string, role uint32) {
	switch role {
	case ssntp.Controller:
		sched.disconnectController(uuid)
	case ssntp.AGENT:
		sched.disconnectComputeNode(uuid)
	case ssntp.NETAGENT:
		sched.disconnectNetworkNode(uuid)
	}

	glog.V(2).Infof("Connect (role 0x%x, uuid=%s)\n", role, uuid)
}

func (sched *ssntpSchedulerServer) StatusNotify(uuid string, status ssntp.Status, frame *ssntp.Frame) {
	payload := frame.Payload

	// for now only pay attention to READY status

	glog.V(2).Infof("STATUS %v from %s\n", status, uuid)

	sched.controllerMutex.RLock()
	defer sched.controllerMutex.RUnlock()
	if sched.controllerMap[uuid] != nil {
		glog.Warningf("Ignoring STATUS change from Controller uuid=%s\n", uuid)
		return
	}

	sched.cnMutex.RLock()
	defer sched.cnMutex.RUnlock()

	sched.nnMutex.RLock()
	defer sched.nnMutex.RUnlock()

	var node *nodeStat
	if sched.cnMap[uuid] != nil {
		node = sched.cnMap[uuid]
	} else if sched.nnMap[uuid] != nil {
		node = sched.nnMap[uuid]
	} else {
		glog.Warningf("STATUS error: no connected ssntp client with uuid=%s\n", uuid)
		return
	}

	node.mutex.Lock()
	defer node.mutex.Unlock()

	node.status = status
	switch node.status {
	case ssntp.READY:
		//pull in client's READY status frame transmitted statistics
		var stats payloads.Ready
		err := yaml.Unmarshal(payload, &stats)
		if err != nil {
			glog.Errorf("Bad READY yaml for node %s\n", uuid)
			return
		}
		node.memTotalMB = stats.MemTotalMB
		node.memAvailMB = stats.MemAvailableMB
		node.load = stats.Load
		node.cpus = stats.CpusOnline
		//TODO pull in other types of payloads.Ready struct data
	}
}

type workResources struct {
	instanceUUID string
	memReqMB     int
	networkNode  int
}

func (sched *ssntpSchedulerServer) getWorkloadResources(work *payloads.Start) (workload workResources, err error) {
	// loop the array to find resources
	for idx := range work.Start.RequestedResources {
		// memory:
		if work.Start.RequestedResources[idx].Type == payloads.MemMB {
			workload.memReqMB = work.Start.RequestedResources[idx].Value
		}

		// network node
		if work.Start.RequestedResources[idx].Type == payloads.NetworkNode {
			workload.networkNode = work.Start.RequestedResources[idx].Value
		}

		// etc...
	}

	// validate the found resources
	if workload.memReqMB <= 0 {
		return workload, fmt.Errorf("invalid start payload resource demand: mem_mb (%d) <= 0, must be > 0", workload.memReqMB)
	}
	if workload.networkNode != 0 && workload.networkNode != 1 {
		return workload, fmt.Errorf("invalid start payload resource demand: network_node (%d) is not 0 or 1", workload.networkNode)
	}

	return workload, nil
}

// Check resource demands are satisfiable by the referenced, locked nodeStat object
func (sched *ssntpSchedulerServer) workloadFits(node *nodeStat, workload *workResources) bool {
	// simple scheduling policy == first memory fit
	if node.memAvailMB >= workload.memReqMB &&
		node.status == ssntp.READY {
		return true
	}
	return false
}

func (sched *ssntpSchedulerServer) sendStartFailureError(clientUUID string, instanceUUID string, reason payloads.StartFailureReason) {
	error := payloads.ErrorStartFailure{
		InstanceUUID: instanceUUID,
		Reason:       reason,
	}

	payload, err := yaml.Marshal(&error)
	if err != nil {
		glog.Errorf("Unable to Marshall Status %v", err)
		return
	}

	glog.Errorf("Unable to dispatch: %v\n", reason)
	sched.ssntp.SendError(clientUUID, ssntp.StartFailure, payload)
}
func (sched *ssntpSchedulerServer) getConcentratorUUID(event ssntp.Event, payload []byte) (string, error) {
	switch event {
	default:
		return "", fmt.Errorf("unsupported ssntp.Event type \"%s\"", event)
	case ssntp.TenantAdded:
		var ev payloads.EventTenantAdded
		err := yaml.Unmarshal(payload, &ev)
		return ev.TenantAdded.ConcentratorUUID, err
	case ssntp.TenantRemoved:
		var ev payloads.EventTenantRemoved
		err := yaml.Unmarshal(payload, &ev)
		return ev.TenantRemoved.ConcentratorUUID, err
	case ssntp.PublicIPAssigned:
		var ev payloads.EventPublicIPAssigned
		err := yaml.Unmarshal(payload, &ev)
		return ev.AssignedIP.ConcentratorUUID, err
	}
}

func (sched *ssntpSchedulerServer) fwdEventToCNCI(event ssntp.Event, payload []byte) (dest ssntp.ForwardDestination) {
	// since the scheduler is the primary ssntp server, it needs to
	// unwrap event payloads and forward them to the approriate recipient

	concentratorUUID, err := sched.getConcentratorUUID(event, payload)
	if err != nil || concentratorUUID == "" {
		glog.Errorf("Bad %s event yaml from, concentratorUUID == %s\n", event, concentratorUUID)
		dest.SetDecision(ssntp.Discard)
		return
	}

	glog.V(2).Infof("Forwarding %s to %s\n", event.String(), concentratorUUID)
	dest.AddRecipient(concentratorUUID)

	return dest
}

func (sched *ssntpSchedulerServer) getWorkloadAgentUUID(command ssntp.Command, payload []byte) (string, string, error) {
	switch command {
	default:
		return "", "", fmt.Errorf("unsupported ssntp.Command type \"%s\"", command)
	case ssntp.RESTART:
		var cmd payloads.Restart
		err := yaml.Unmarshal(payload, &cmd)
		return cmd.Restart.InstanceUUID, cmd.Restart.WorkloadAgentUUID, err
	case ssntp.STOP:
		var cmd payloads.Stop
		err := yaml.Unmarshal(payload, &cmd)
		return cmd.Stop.InstanceUUID, cmd.Stop.WorkloadAgentUUID, err
	case ssntp.DELETE:
		var cmd payloads.Delete
		err := yaml.Unmarshal(payload, &cmd)
		return cmd.Delete.InstanceUUID, cmd.Delete.WorkloadAgentUUID, err
	case ssntp.EVACUATE:
		var cmd payloads.Evacuate
		err := yaml.Unmarshal(payload, &cmd)
		return "", cmd.Evacuate.WorkloadAgentUUID, err
	}
}

func (sched *ssntpSchedulerServer) fwdCmdToComputeNode(command ssntp.Command, payload []byte) (dest ssntp.ForwardDestination, instanceUUID string) {
	// some commands require no scheduling choice, rather the specified
	// agent/launcher needs the command instead of the scheduler
	instanceUUID, cnDestUUID, err := sched.getWorkloadAgentUUID(command, payload)
	if err != nil || cnDestUUID == "" {
		glog.Errorf("Bad %s command yaml from Controller, WorkloadAgentUUID == %s\n", command.String(), cnDestUUID)
		dest.SetDecision(ssntp.Discard)
		return
	}

	glog.V(2).Infof("Forwarding controller %s command to %s\n", command.String(), cnDestUUID)
	dest.AddRecipient(cnDestUUID)

	return
}

// Decrement resource claims for the referenced locked nodeStat object
func (sched *ssntpSchedulerServer) decrementResourceUsage(node *nodeStat, workload *workResources) {
	node.memAvailMB -= workload.memReqMB
}

// Find suitable compute node, returning referenced to a locked nodeStat if found
func (sched *ssntpSchedulerServer) pickComputeNode(controllerUUID string, workload *workResources) (node *nodeStat) {
	sched.cnMutex.RLock()
	defer sched.cnMutex.RUnlock()

	if len(sched.cnList) == 0 {
		sched.sendStartFailureError(controllerUUID, workload.instanceUUID, payloads.NoComputeNodes)
		return nil
	}

	/* Shortcut for 1 nodes cluster */
	if len(sched.cnList) == 1 {
		node := sched.cnList[0]
		node.mutex.Lock()
		if sched.workloadFits(sched.cnList[0], workload) == true {
			node.mutex.Unlock()
			return node
		}
		node.mutex.Unlock()
		return nil
	}

	/* First try nodes after the MRU */
	if sched.cnMRUIndex != -1 && sched.cnMRUIndex < len(sched.cnList)-1 {
		for i, node := range sched.cnList[sched.cnMRUIndex+1:] {
			node.mutex.Lock()
			if node == sched.cnMRU {
				node.mutex.Unlock()
				continue
			}

			if sched.workloadFits(node, workload) == true {
				sched.cnMRUIndex = sched.cnMRUIndex + 1 + i
				sched.cnMRU = node
				node.mutex.Unlock()
				return node
			}
			node.mutex.Unlock()
		}
	}

	/* Then try the whole list, including the MRU */
	for i, node := range sched.cnList {
		node.mutex.Lock()
		if sched.workloadFits(node, workload) == true {
			sched.cnMRUIndex = i
			sched.cnMRU = node
			node.mutex.Unlock()
			return node
		}
		node.mutex.Unlock()
	}

	sched.sendStartFailureError(controllerUUID, workload.instanceUUID, payloads.FullCloud)
	return nil
}

// Find suitable net node, returning referenced to a locked nodeStat if found
func (sched *ssntpSchedulerServer) pickNetworkNode(controllerUUID string, workload *workResources) (node *nodeStat) {
	sched.nnMutex.RLock()
	defer sched.nnMutex.RUnlock()

	if len(sched.nnMap) == 0 {
		sched.sendStartFailureError(controllerUUID, workload.instanceUUID, payloads.NoNetworkNodes)
		return nil
	}

	// with more than one node MRU gives simplistic spread
	for _, node := range sched.nnMap {
		node.mutex.Lock()
		if (len(sched.nnMap) <= 1 || ((len(sched.nnMap) > 1) && (node.uuid != sched.nnMRU))) &&
			sched.workloadFits(node, workload) {
			sched.nnMRU = node.uuid
			node.mutex.Unlock()
			return node
		}
	}

	sched.sendStartFailureError(controllerUUID, workload.instanceUUID, payloads.NoNetworkNodes)
	return nil
}

func (sched *ssntpSchedulerServer) startWorkload(controllerUUID string, payload []byte) (dest ssntp.ForwardDestination, instanceUUID string) {
	var work payloads.Start
	err := yaml.Unmarshal(payload, &work)
	if err != nil {
		glog.Errorf("Bad START workload yaml from Controller %s: %s\n", controllerUUID, err)
		dest.SetDecision(ssntp.Discard)
		return dest, ""
	}

	workload, err := sched.getWorkloadResources(&work)
	if err != nil {
		glog.Errorf("Bad START workload resource list from Controller %s: %s\n", controllerUUID, err)
		dest.SetDecision(ssntp.Discard)
		return dest, ""
	}

	instanceUUID = workload.instanceUUID

	var targetNode *nodeStat

	if workload.networkNode == 0 {
		targetNode = sched.pickComputeNode(controllerUUID, &workload)
	} else { //workload.network_node == 1
		targetNode = sched.pickNetworkNode(controllerUUID, &workload)
	}

	if targetNode != nil {
		//TODO: mark the targetNode as unavailable until next stats / READY checkin?
		//	or is subtracting mem demand sufficiently speculative enough?
		//	Goal is to have spread, not schedule "too many" workloads back
		//	to back on the same targetNode, but also not add latency to dispatch and
		//	hopefully not queue when all nodes have just started a workload.
		sched.decrementResourceUsage(targetNode, &workload)

		dest.AddRecipient(targetNode.uuid)
		targetNode.mutex.Unlock()
	} else {
		// TODO Queue the frame ?
		dest.SetDecision(ssntp.Discard)
	}

	return dest, instanceUUID
}

func (sched *ssntpSchedulerServer) CommandForward(controllerUUID string, command ssntp.Command, frame *ssntp.Frame) (dest ssntp.ForwardDestination) {
	payload := frame.Payload
	instanceUUID := ""

	sched.controllerMutex.RLock()
	defer sched.controllerMutex.RUnlock()
	if sched.controllerMap[controllerUUID] == nil {
		glog.Warningf("Ignoring %s command from unknown Controller %s\n", command, controllerUUID)
		dest.SetDecision(ssntp.Discard)
		return
	}
	controller := sched.controllerMap[controllerUUID]
	controller.mutex.Lock()
	if controller.status != controllerMaster {
		glog.Warningf("Ignoring %s command from non-master Controller %s\n", command, controllerUUID)
		dest.SetDecision(ssntp.Discard)
		controller.mutex.Unlock()
		return
	}
	controller.mutex.Unlock()

	start := time.Now()

	glog.V(2).Infof("Command %s from %s\n", command, controllerUUID)

	switch command {
	// the main command with scheduler processing
	case ssntp.START:
		dest, instanceUUID = sched.startWorkload(controllerUUID, payload)
	case ssntp.RESTART:
		fallthrough
	case ssntp.STOP:
		fallthrough
	case ssntp.DELETE:
		fallthrough
	case ssntp.EVACUATE:
		dest, instanceUUID = sched.fwdCmdToComputeNode(command, payload)
	default:
		dest.SetDecision(ssntp.Discard)
	}

	elapsed := time.Since(start)
	glog.V(2).Infof("%s command processed for instance %s in %s\n", command, instanceUUID, elapsed)

	return
}

func (sched *ssntpSchedulerServer) CommandNotify(uuid string, command ssntp.Command, frame *ssntp.Frame) {
	// Currently all commands are handled by CommandForward, the SSNTP command forwader,
	// or directly by role defined forwarding rules.
	glog.V(2).Infof("COMMAND %v from %s\n", command, uuid)
}

func (sched *ssntpSchedulerServer) EventForward(uuid string, event ssntp.Event, frame *ssntp.Frame) (dest ssntp.ForwardDestination) {
	payload := frame.Payload

	start := time.Now()

	switch event {
	case ssntp.TenantAdded:
		fallthrough
	case ssntp.TenantRemoved:
		fallthrough
	case ssntp.PublicIPAssigned:
		dest = sched.fwdEventToCNCI(event, payload)
	}

	elapsed := time.Since(start)
	glog.V(2).Infof("%s event processed for instance %s in %s\n", event.String(), uuid, elapsed)

	return dest
}

func (sched *ssntpSchedulerServer) EventNotify(uuid string, event ssntp.Event, frame *ssntp.Frame) {
	// Currently all events are handled by EventForward, the SSNTP command forwader,
	// or directly by role defined forwarding rules.
	glog.V(2).Infof("EVENT %v from %s\n", event, uuid)
}

func (sched *ssntpSchedulerServer) ErrorNotify(uuid string, error ssntp.Error, frame *ssntp.Frame) {
	glog.V(2).Infof("ERROR %v from %s\n", error, uuid)
}

func setLimits() {
	var rlim syscall.Rlimit
	err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rlim)
	if err != nil {
		glog.Warningf("Getrlimit failed %v", err)
		return
	}

	glog.Infof("Initial nofile limits: cur %d max %d", rlim.Cur, rlim.Max)

	if rlim.Cur < rlim.Max {
		oldCur := rlim.Cur
		rlim.Cur = rlim.Max
		err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &rlim)
		if err != nil {
			glog.Warningf("Setrlimit failed %v", err)
			rlim.Cur = oldCur
		}
	}

	glog.Infof("Updated nofile limits: cur %d max %d", rlim.Cur, rlim.Max)
}

func heartBeatControllers(sched *ssntpSchedulerServer) (s string) {
	// show the first two controller's
	controllerMax := 2
	i := 0

	sched.controllerMutex.RLock()
	for _, controller := range sched.controllerMap {
		controller.mutex.Lock()
		s += fmt.Sprintf("controller-%s:", controller.uuid[:8])
		s += controller.status.String()
		controller.mutex.Unlock()

		i++
		if i == controllerMax {
			break
		}
		if i <= controllerMax && len(sched.controllerMap) > i {
			s += ", "
		} else {
			s += "\t"
		}
	}
	sched.controllerMutex.RUnlock()

	if i == 0 {
		s += " -no Controller- \t\t\t\t\t"
	} else if i < controllerMax {
		s += "\t\t\t"
	} else {
		s += "\t"
	}

	return s
}

func heartBeatComputeNodes(sched *ssntpSchedulerServer) (s string) {
	// show the first four compute nodes
	cnMax := 4
	i := 0

	sched.cnMutex.RLock()
	for _, node := range sched.cnMap {

		node.mutex.Lock()
		s += fmt.Sprintf("node-%s:", node.uuid[:8])
		s += node.status.String()
		if node == sched.cnMRU {
			s += "*"
		}
		s += ":" + fmt.Sprintf("%d/%d,%d",
			node.memAvailMB,
			node.memTotalMB,
			node.load)
		node.mutex.Unlock()

		i++
		if i == cnMax {
			break
		}
		if i <= cnMax && len(sched.cnMap) > i {
			s += ", "
		}
	}
	sched.cnMutex.RUnlock()

	if i == 0 {
		s += " -no Compute Nodes-"
	}

	return s
}

func heartBeat(sched *ssntpSchedulerServer) {
	iter := 0
	for {
		var beatTxt string

		time.Sleep(time.Duration(1) * time.Second)

		sched.controllerMutex.RLock()
		sched.cnMutex.RLock()
		if len(sched.controllerMap) == 0 && len(sched.cnMap) == 0 {
			beatTxt = "** idle / disconnected **"
		}
		sched.controllerMutex.RUnlock()
		sched.cnMutex.RUnlock()

		iter++
		if iter%22 == 0 {
			//output a column indication occasionally
			log.Printf("Controllers\t\t\t\t\tCompute Nodes\n")
		}

		beatTxt = heartBeatControllers(sched)
		beatTxt += heartBeatComputeNodes(sched)

		log.Printf("%s\n", beatTxt)
	}
}

func main() {
	var cert = flag.String("cert", "/etc/pki/ciao/cert-server-localhost.pem", "Server certificate")
	var CAcert = flag.String("cacert", "/etc/pki/ciao/CAcert-server-localhost.pem", "CA certificate")
	var cpuprofile = flag.String("cpuprofile", "", "Write cpu profile to file")
	var heartbeat = flag.Bool("heartbeat", false, "Emit status heartbeat text")
	var logDir = "/var/lib/ciao/logs/scheduler"

	flag.Parse()

	logDirFlag := flag.Lookup("log_dir")
	if logDirFlag == nil {
		glog.Errorf("log_dir does not exist")
		return
	}
	if logDirFlag.Value.String() == "" {
		logDirFlag.Value.Set(logDir)
	}
	if err := os.MkdirAll(logDirFlag.Value.String(), 0755); err != nil {
		glog.Errorf("Unable to create log directory (%s) %v", logDir, err)
		return
	}

	setLimits()

	sched := newSsntpSchedulerServer()

	if len(*cpuprofile) != 0 {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			log.Print(err)
		}
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}

	//config.Trace = os.Stdout
	//config.Error = os.Stdout
	//config.DebugInterface = false

	config := &ssntp.Config{
		CAcert: *CAcert,
		Cert:   *cert,
		Role:   ssntp.SCHEDULER,
	}

	config.ForwardRules = []ssntp.FrameForwardRule{
		{ // all STATS commands go to all Controllers
			Operand: ssntp.STATS,
			Dest:    ssntp.Controller,
		},
		{ // all TraceReport events go to all Controllers
			Operand: ssntp.TraceReport,
			Dest:    ssntp.Controller,
		},
		{ // all InstanceDeleted events go to all Controllers
			Operand: ssntp.InstanceDeleted,
			Dest:    ssntp.Controller,
		},
		{ // all ConcentratorInstanceAdded events go to all Controllers
			Operand: ssntp.ConcentratorInstanceAdded,
			Dest:    ssntp.Controller,
		},
		{ // all StartFailure events go to all Controllers
			Operand: ssntp.StartFailure,
			Dest:    ssntp.Controller,
		},
		{ // all StopFailure events go to all Controllers
			Operand: ssntp.StopFailure,
			Dest:    ssntp.Controller,
		},
		{ // all RestartFailure events go to all Controllers
			Operand: ssntp.RestartFailure,
			Dest:    ssntp.Controller,
		},
		{ // all START command are processed by the Command forwarder
			Operand:        ssntp.START,
			CommandForward: sched,
		},
		{ // all RESTART command are processed by the Command forwarder
			Operand:        ssntp.RESTART,
			CommandForward: sched,
		},
		{ // all STOP command are processed by the Command forwarder
			Operand:        ssntp.STOP,
			CommandForward: sched,
		},
		{ // all DELETE command are processed by the Command forwarder
			Operand:        ssntp.DELETE,
			CommandForward: sched,
		},
		{ // all EVACUATE command are processed by the Command forwarder
			Operand:        ssntp.EVACUATE,
			CommandForward: sched,
		},
		{ // all TenantAdded events are processed by the Event forwarder
			Operand:      ssntp.TenantAdded,
			EventForward: sched,
		},
		{ // all TenantRemoved events are processed by the Event forwarder
			Operand:      ssntp.TenantRemoved,
			EventForward: sched,
		},
		{ // all PublicIPAssigned events are processed by the Event forwarder
			Operand:      ssntp.PublicIPAssigned,
			EventForward: sched,
		},
	}

	if *heartbeat {
		go heartBeat(sched)
	}

	sched.ssntp.Serve(config, sched)
}
