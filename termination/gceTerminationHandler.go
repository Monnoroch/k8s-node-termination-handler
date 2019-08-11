// Copyright 2017 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package termination

import (
	"sync"
	"time"

	"github.com/golang/glog"

	"cloud.google.com/go/compute/metadata"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	terminateForMaintenance            = "TERMINATE"
	maintenanceEventTerminate          = "TERMINATE_ON_HOST_MAINTENANCE"
	maintenanceEventTrue               = "TRUE"
	maintenanceEventSuffix             = "instance/maintenance-event"
	preemptedEventSuffix               = "instance/preempted"
	preemptibleNodeTerminationDuration = 30 * time.Second
)

type gceTerminationSource struct {
	sync.RWMutex
	state         NodeTerminationState
	updateChannel chan NodeTerminationState
	nodeName      string
}

func NewGCETerminationSource(nodeName string) (NodeTerminationSource, error) {
	ret := &gceTerminationSource{
		updateChannel: make(chan NodeTerminationState),
		nodeName:      nodeName,
	}
	var err error
	// Check if a termination is already pending. This can happen if the termination watcher restarts.
	pendingTermination, err := pendingTermination()
	if err != nil {
		return nil, err
	}
	// Store a pending termination.
	if pendingTermination {
		ret.storePendingTermination()
	}
	return ret, nil
}

func pendingTermination() (bool, error) {
	state, err := metadata.Get(maintenanceEventSuffix)
	if err != nil {
		return false, err
	}
	pvmState, err := metadata.Get(preemptedEventSuffix)
	if err != nil {
		return false, err
	}
	glog.V(4).Infof("Current states: Regular: %q, PVM: %q", state, pvmState)
	return (state == maintenanceEventTerminate || pvmState == maintenanceEventTrue), nil
}

func (g *gceTerminationSource) storePendingTermination() {
	g.Lock()
	defer g.Unlock()

	g.state.PendingTermination = true
	terminationTime := time.Now()
	// This is a Preemptible node
	g.state.TerminationTime = terminationTime.Add(preemptibleNodeTerminationDuration)
}

func (g *gceTerminationSource) resetPendingTermination() {
	g.Lock()
	defer g.Unlock()

	g.state.PendingTermination = false
	g.state.TerminationTime = time.Now()
}

func (g *gceTerminationSource) handleMaintenanceEvents(state string, exists bool) error {
	if !exists {
		glog.Errorf("Maintenance Event Metadata API deleted unexpectedly")
		return nil
	}
	glog.Infof("Handling maintenance event with state: %q", state)

	// Regular GPU VMs are expected to observe `TERMINATE_ON_HOST_MAINTENANCE` on `maintenance-event` metadata variable.
	// PVMs are expected to observe `TRUE` on `preempted` metadata variable.
	if state == maintenanceEventTrue {
		glog.Infof("Recording impending termination")
		g.storePendingTermination()
		g.updateChannel <- g.state
	} else {
		glog.Infof("Removing any impending termination records")
		g.resetPendingTermination()
		g.updateChannel <- g.state
	}
	return nil
}

func (g *gceTerminationSource) WatchState() <-chan NodeTerminationState {
	go wait.Forever(func() {
		err := metadata.Subscribe(maintenanceEventSuffix, g.handleMaintenanceEvents)
		if err != nil {
			glog.Errorf("Failed to get maintenance status for node %q - %v", g.nodeName, err)
			return
		}
	}, time.Second)
	go wait.Forever(func() {
		err := metadata.Subscribe(preemptedEventSuffix, g.handleMaintenanceEvents)
		if err != nil {
			glog.Errorf("Failed to get preemptible maintenance status for node %q - %v", g.nodeName, err)
			return
		}
	}, time.Second)
	return g.updateChannel
}

func (g *gceTerminationSource) GetState() NodeTerminationState {
	g.RLock()
	defer g.RUnlock()
	return g.state
}
