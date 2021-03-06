// Copyright (c) 2019 Uber Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package hostcache

import (
	"sync"
	"time"

	peloton "github.com/uber/peloton/.gen/peloton/api/v1alpha/peloton"
	hostmgr "github.com/uber/peloton/.gen/peloton/private/hostmgr/v1alpha"
	"github.com/uber/peloton/pkg/common/lifecycle"
	"github.com/uber/peloton/pkg/hostmgr/p2k/plugins"
	"github.com/uber/peloton/pkg/hostmgr/p2k/scalar"
	hmscalar "github.com/uber/peloton/pkg/hostmgr/scalar"

	log "github.com/sirupsen/logrus"
	"go.uber.org/multierr"
	"go.uber.org/yarpc/yarpcerrors"
)

// HostCache manages cluster resources, and provides necessary abstractions to
// interact with underlying system.
type HostCache interface {
	// AcquireLeases acquires leases on hosts that match the filter constraints.
	AcquireLeases(hostFilter *hostmgr.HostFilter) ([]*hostmgr.HostLease, map[string]uint32)

	// TerminateLease is called when the lease is not going to be used, and we
	// want to release the lock on the host.
	TerminateLease(hostname string, leaseID string) error

	// CompleteLease is called when launching pods on a host that has been
	// previously leased to the Placement engine.
	CompleteLease(hostname string, leaseID string, podToResMap map[string]hmscalar.Resources) error

	// GetClusterCapacity gets the total capacity and allocation of the cluster.
	GetClusterCapacity() (capacity, allocation hmscalar.Resources)

	// Start will start the goroutine that listens for host events.
	Start()

	// Stop will stop the host cache go routine that listens for host events.
	Stop()

	// GetSummaries returns a list of host summaries that the host cache is
	// managing.
	GetSummaries() (summaries []HostSummary)

	// ResetExpiredHeldHostSummaries resets the status of each hostSummary if
	// the helds have expired and returns the hostnames which got reset.
	ResetExpiredHeldHostSummaries(now time.Time) []string

	// GetHostHeldForPod returns the host that is held for the pod.
	GetHostHeldForPod(podID *peloton.PodID) string

	// HoldForPods holds the host for the pods specified.
	HoldForPods(hostname string, podIDs []*peloton.PodID) error

	// ReleaseHoldForPods release the hold of host for the pods specified.
	ReleaseHoldForPods(hostname string, podIDs []*peloton.PodID) error
}

// hostCache is an implementation of HostCache interface.
type hostCache struct {
	mu sync.RWMutex

	// Map of hostname to HostSummary.
	hostIndex map[string]HostSummary

	// Map of podID to host held.
	podHeldIndex map[string]string

	// The event channel on which the underlying cluster manager plugin will send
	// host events to host cache.
	hostEventCh chan *scalar.HostEvent

	// The event channel on which the underlying cluster manager plugin will send
	// pod events to host cache.
	podEventCh chan *scalar.PodEvent

	// Cluster manager plugin.
	plugin plugins.Plugin

	// Lifecycle manager.
	lifecycle lifecycle.LifeCycle
}

// New returns a new instance of host cache.
func New(
	hostEventCh chan *scalar.HostEvent,
	podEventCh chan *scalar.PodEvent,
	plugin plugins.Plugin,
) HostCache {
	return &hostCache{
		hostIndex:   make(map[string]HostSummary),
		hostEventCh: hostEventCh,
		podEventCh:  podEventCh,
		plugin:      plugin,
		lifecycle:   lifecycle.NewLifeCycle(),
	}
}

func (c *hostCache) GetSummaries() []HostSummary {
	c.mu.RLock()
	defer c.mu.RUnlock()

	summaries := make([]HostSummary, 0, len(c.hostIndex))
	for _, summary := range c.hostIndex {
		summaries = append(summaries, summary)
	}
	return summaries
}

// AcquireLeases acquires leases on hosts that match the filter constraints.
// The lease will be held until Jobmgr actively launches pods using the leaseID.
// Returns:
// []*hostmgr.HostLease: List of leases acquired on matching hosts.
// map[string]uint32: map filtering result string (i.e. HOST_FILTER_INVALID) to
// number of hosts per result for debugging purpose.
func (c *hostCache) AcquireLeases(
	hostFilter *hostmgr.HostFilter,
) ([]*hostmgr.HostLease, map[string]uint32) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	matcher := NewMatcher(hostFilter)

	// If host hint is provided, try to return the hosts in hints first.
	for _, filterHints := range hostFilter.GetHint().GetHostHint() {
		if hs, ok := c.hostIndex[filterHints.GetHostname()]; ok {
			matcher.tryMatch(hs.GetHostname(), hs)
			if matcher.hostLimitReached() {
				break
			}
		}
	}

	// TODO: implement defrag/firstfit ranker, for now default to first fit
	for hostname, hs := range c.hostIndex {
		matcher.tryMatch(hostname, hs)
		if matcher.hostLimitReached() {
			break
		}
	}

	var hostLeases []*hostmgr.HostLease
	hostLimitReached := matcher.hostLimitReached()
	for _, hostname := range matcher.hostNames {
		hs := c.hostIndex[hostname]
		hostLeases = append(hostLeases, hs.GetHostLease())
	}

	if !hostLimitReached {
		// Still proceed to return something.
		log.WithFields(log.Fields{
			"host_filter":         hostFilter,
			"matched_host_leases": hostLeases,
			"match_result_counts": matcher.filterCounts,
		}).Debug("Number of hosts matched is fewer than max hosts")
	}
	return hostLeases, matcher.filterCounts
}

// TerminateLease is called when a lease that was previously acquired, and a
// host locked, is no longer in use. The leaseID of the acquired host should be
// supplied in this call so that the hostcache can match the leaseID.
// At this point, the existing lease is terminated and the host can be used for
// further placement.
// Error cases:
//		LeaseID doesn't match
//		Host is not in Placing status
func (c *hostCache) TerminateLease(
	hostname string,
	leaseID string,
) error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	hs, err := c.getSummary(hostname)
	if err != nil {
		return err
	}
	if err := hs.TerminateLease(leaseID); err != nil {
		// TODO: metrics
		return err
	}
	return nil
}

// CompleteLease is called when launching pods on a host that has been
// previously leased to the Placement engine. The leaseID of the acquired host
// should be supplied in this call so that the hostcache can match the leaseID,
// verify that sufficient resources are present on the host to launch all the
// pods in podToResMap, and then allow the pods to be launched on this host.
// At this point, the existing lease is Completed and the host can be used for
// further placement.
// Error cases:
// 		LeaseID doesn't match
//		Host is not in Placing status
// 		There are insufficient resources on the requested host
func (c *hostCache) CompleteLease(
	hostname string,
	leaseID string,
	podToResMap map[string]hmscalar.Resources,
) error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	hs, err := c.getSummary(hostname)
	if err != nil {
		return err
	}
	if err := hs.CompleteLease(leaseID, podToResMap); err != nil {
		// TODO: metrics
		return err
	}

	// TODO: remove held hosts.
	return nil
}

// GetClusterCapacity gets the total cluster capacity and allocation
func (c *hostCache) GetClusterCapacity() (
	capacity, allocation hmscalar.Resources,
) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	// Go through the hostIndex and calculate capacity and allocation
	// and sum it up to get these at a cluster level
	for _, hs := range c.hostIndex {
		capacity = capacity.Add(hs.GetCapacity())
		allocation = allocation.Add(hs.GetAllocated())
	}
	return
}

// ResetExpiredHeldHostSummaries resets the status of each hostSummary if
// the holds have expired and returns the hostnames which got reset.
func (c *hostCache) ResetExpiredHeldHostSummaries(deadline time.Time) []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	var ret []string
	for hostname, hs := range c.hostIndex {
		isFreed, _, podIDExpired := hs.DeleteExpiredHolds(deadline)
		if isFreed {
			ret = append(ret, hostname)
			// TODO: add metrics.
		}
		for _, id := range podIDExpired {
			c.removePodHold(id)
		}
	}
	return ret
}

func (c *hostCache) GetHostHeldForPod(podID *peloton.PodID) string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	hn, ok := c.podHeldIndex[podID.GetValue()]
	if !ok {
		// TODO: this should return an error. But keep it the same way as in
		// offerpool for now.
		return ""
	}
	return hn
}

func (c *hostCache) HoldForPods(hostname string, podIDs []*peloton.PodID) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	hs, err := c.getSummary(hostname)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range podIDs {
		if err := hs.HoldForPod(id); err != nil {
			errs = append(errs, err)
			continue
		}
		c.addPodHold(hostname, id)
	}
	if len(errs) > 0 {
		return yarpcerrors.InternalErrorf("failed to hold pods: %s", multierr.Combine(errs...))
	}
	return nil
}

func (c *hostCache) ReleaseHoldForPods(hostname string, podIDs []*peloton.PodID) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	hs, err := c.getSummary(hostname)
	if err != nil {
		return err
	}
	for _, id := range podIDs {
		hs.ReleaseHoldForPod(id)
		c.removePodHold(id)
	}
	return nil
}

// addPodHold add a pod to podHeldIndex. Replace the old host if exists.
func (c *hostCache) addPodHold(hostname string, id *peloton.PodID) {
	old, ok := c.podHeldIndex[id.GetValue()]
	if ok && old != hostname {
		log.WithFields(log.Fields{
			"new_host": hostname,
			"old_host": old,
			"task_id":  id.GetValue(),
		}).Warn("pod is held by multiple hosts")
	}
	c.podHeldIndex[id.GetValue()] = hostname
}

// removePodHold deletes id from podHeldIndex regardless of hostname.
func (c *hostCache) removePodHold(id *peloton.PodID) {
	delete(c.podHeldIndex, id.GetValue())
}

// getSummary returns host summary given name. If the host does not exist,
// return error not found.
func (c *hostCache) getSummary(hostname string) (HostSummary, error) {
	hs, ok := c.hostIndex[hostname]
	if !ok {
		// TODO: metrics
		return nil, yarpcerrors.NotFoundErrorf("cannot find host %s in cache", hostname)
	}
	return hs, nil
}

// waitForHostEvents will start a goroutine that waits on the host events
// channel. The underlying plugin will send events to this channel when any
// underlying host status changes. Example: allocated resources change,
// host goes down or is put in maintenance mode.
func (c *hostCache) waitForHostEvents() {
	for {
		select {
		case event := <-c.hostEventCh:
			switch event.GetEventType() {
			case scalar.AddHost:
				c.addHost(event)
			case scalar.UpdateHostSpec:
				c.updateHostSpec(event)
			case scalar.DeleteHost:
				c.deleteHost(event)
			case scalar.UpdateHostAvailableRes:
				c.updateHostAvailable(event)
			}
		case <-c.lifecycle.StopCh():
			return
		}
	}
}

// waitForPodEvents will start a goroutine that waits on the pod events
// channel. The underlying plugin manager will send events to this channel
// when a pod status changes. Example: pod P1 on host H1 was deleted, or
// evicted.
func (c *hostCache) waitForPodEvents() {
	for {
		select {
		case event := <-c.podEventCh:
			c.handlePodEvent(event)
		case <-c.lifecycle.StopCh():
			return
		}
	}
}

func (c *hostCache) handlePodEvent(event *scalar.PodEvent) {
	// TODO: evaluate locking strategy
	c.mu.Lock()
	defer c.mu.Unlock()

	hostname := event.Event.GetHostname()
	summary, found := c.hostIndex[hostname]
	if !found {
		// TODO(pourchet): Figure out how to handle this.
		// This could happen if reconciliation was not done before
		// we start processing pod events.
		log.WithFields(log.Fields{
			"hostname": hostname,
			"pod_id":   event.Event.GetPodId().GetValue(),
		}).Error("delete pod event ignored: host summary not found")
		return
	}

	err := summary.HandlePodEvent(event)
	if err != nil {
		log.WithFields(log.Fields{
			"hostname": hostname,
			"pod_id":   event.Event.GetPodId().GetValue(),
		}).WithError(err).Error("handle pod event")
	}
}

func (c *hostCache) addHost(event *scalar.HostEvent) {
	// TODO: evaluate locking strategy
	c.mu.Lock()
	defer c.mu.Unlock()

	hostInfo := event.GetHostInfo()
	version := hostInfo.GetResourceVersion()
	capacity := hostInfo.GetCapacity()

	// Check if the host already exists in the cache and reject if the event is
	// of older version.
	if existing, ok := c.hostIndex[hostInfo.GetHostName()]; ok {
		evtVersion := hostInfo.GetResourceVersion()

		// Check if event has older resource version, ignore if it does
		currentVersion := existing.GetVersion()
		if scalar.IsOldVersion(currentVersion, evtVersion) {
			log.WithFields(log.Fields{
				"hostname":        hostInfo.GetHostName(),
				"capacity":        capacity,
				"event_version":   evtVersion,
				"current_version": currentVersion,
			}).Debug("ignore add event")
			return
		}
	}

	// TODO: figure out how to differemtiate mesos/k8s hosts,
	// now addHost is only used by k8s hosts
	c.hostIndex[hostInfo.GetHostName()] = newKubeletHostSummary(
		hostInfo.GetHostName(),
		capacity,
		version,
	)
	log.WithFields(log.Fields{
		"hostname": hostInfo.GetHostName(),
		"capacity": hostInfo.GetCapacity(),
		"version":  version,
	}).Debug("add host to cache")
}

func (c *hostCache) updateHostSpec(event *scalar.HostEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var hs HostSummary
	var ok bool

	hostInfo := event.GetHostInfo()
	evtVersion := hostInfo.GetResourceVersion()
	capacity := hostInfo.GetCapacity()

	if hs, ok = c.hostIndex[hostInfo.GetHostName()]; !ok {
		// Host not found, possibly an out of order even during host
		// maintenance, due to host being removed from host manager before API
		// server.
		// If for some reason a host was indeed missing, it will be added via
		// reconcile logic.
		log.WithFields(log.Fields{
			"hostname":      hostInfo.GetHostName(),
			"capacity":      capacity,
			"event_version": evtVersion,
		}).Debug("ignore update event, host not found in cache")
		return
	}

	// Check if event has older resource version, ignore if it does.
	currentVersion := hs.GetVersion()
	if scalar.IsOldVersion(currentVersion, evtVersion) {
		log.WithFields(log.Fields{
			"hostname":        hostInfo.GetHostName(),
			"capacity":        capacity,
			"event_version":   evtVersion,
			"current_version": currentVersion,
		}).Debug("ignore update event")
		return
	}

	r := hmscalar.FromPelotonResources(capacity)
	hs.SetCapacity(r)
	hs.SetVersion(evtVersion)
	log.WithFields(log.Fields{
		"hostname": hostInfo.GetHostName(),
		"capacity": hostInfo.GetCapacity(),
		"version":  evtVersion,
	}).Debug("update host in cache")
}

func (c *hostCache) deleteHost(event *scalar.HostEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()

	hostInfo := event.GetHostInfo()
	version := hostInfo.GetResourceVersion()

	// Check if the host already exists in the cache and reject if the event is
	// of older version.
	if existing, ok := c.hostIndex[hostInfo.GetHostName()]; ok {
		evtVersion := hostInfo.GetResourceVersion()

		// Check if event has older resource version, ignore if it does.
		currentVersion := existing.GetVersion()
		if scalar.IsOldVersion(currentVersion, evtVersion) {
			log.WithFields(log.Fields{
				"hostname":        hostInfo.GetHostName(),
				"event_version":   evtVersion,
				"current_version": currentVersion,
			}).Debug("ignore delete event")
			return
		}
	}

	delete(c.hostIndex, hostInfo.GetHostName())
	log.WithFields(log.Fields{
		"hostname": hostInfo.GetHostName(),
		"capacity": hostInfo.GetCapacity(),
		"version":  version,
	}).Debug("delete host from cache")
}

// only applicable to mesos
func (c *hostCache) updateHostAvailable(event *scalar.HostEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var hs HostSummary
	var ok bool

	hostInfo := event.GetHostInfo()
	evtVersion := hostInfo.GetResourceVersion()

	hs, ok = c.hostIndex[hostInfo.GetHostName()]

	if !ok {
		hs = newMesosHostSummary(hostInfo.GetHostName(), evtVersion)
		c.hostIndex[hostInfo.GetHostName()] = hs
	}

	r := hmscalar.FromPelotonResources(hostInfo.GetAvailable())
	hs.SetAvailable(r)
	hs.SetVersion(evtVersion)
	log.WithFields(log.Fields{
		"hostname":  hostInfo.GetHostName(),
		"available": hostInfo.GetAvailable(),
		"version":   evtVersion,
	}).Debug("update host in cache")
}

// Start will start the goroutine that listens for host events.
func (c *hostCache) Start() {
	if !c.lifecycle.Start() {
		return
	}

	go c.waitForHostEvents()
	go c.waitForPodEvents()

	log.Warn("hostCache started")
}

// Stop will stop the host cache go routine that listens for host events.
func (c *hostCache) Stop() {
	if !c.lifecycle.Stop() {
		return
	}

	// Wait for drainer to be stopped
	c.lifecycle.Wait()
	log.Info("hostCache stopped")
}

// Reconcile explicitly reconciles host cache.
func (c *hostCache) Reconcile() error {
	// TODO: Implement
	return nil
}
