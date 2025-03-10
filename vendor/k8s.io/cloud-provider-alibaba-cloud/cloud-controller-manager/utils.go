/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package alicloud

import (
	"fmt"
	"k8s.io/api/core/v1"
	"net"
	"sync"
)

// localService is a local cache try to record the max resource version of each service.
// this is a workaround of BUG #https://github.com/kubernetes/kubernetes/issues/59084
var (
	versionCache *localService
	once         sync.Once
)

type localService struct {
	maxResourceVersion map[string]bool
	lock               sync.RWMutex
}

func GetLocalService() *localService {
	once.Do(func() {
		versionCache = &localService{
			maxResourceVersion: map[string]bool{},
		}
	})
	return versionCache
}

func (s *localService) set(serviceUID string) {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.maxResourceVersion[serviceUID] = true
}

func (s *localService) get(serviceUID string) (found bool) {
	s.lock.RLock()
	defer s.lock.RUnlock()
	_, found = s.maxResourceVersion[serviceUID]
	return
}

func NodeList(nodes []*v1.Node) []string {
	ns := []string{}
	for _, node := range nodes {
		ns = append(ns, node.Name)
	}
	return ns
}

func Contains(list []int, x int) bool {
	for _, item := range list {
		if item == x {
			return true
		}
	}
	return false
}

func RealContainsCidr(outer string, inner string) (bool, error) {
	contains, err := ContainsCidr(outer, inner)
	if err != nil {
		return false, err
	}
	if outer == inner {
		return false, nil
	}
	return contains, nil
}

func ContainsCidr(outer string, inner string) (bool, error) {
	_, outerCidr, err := net.ParseCIDR(outer)
	if err != nil {
		return false, fmt.Errorf("error parse outer cidr: %s, message: %s", outer, err.Error())
	}
	_, innerCidr, err := net.ParseCIDR(inner)
	if err != nil {
		return false, fmt.Errorf("error parse inner cidr: %s, message: %s", inner, err.Error())
	}

	lastIP := make([]byte, len(innerCidr.IP))
	for i := range lastIP {
		lastIP[i] = innerCidr.IP[i] | ^innerCidr.Mask[i]
	}
	if !outerCidr.Contains(innerCidr.IP) || !outerCidr.Contains(lastIP) {
		return false, nil
	}
	return true, nil
}
