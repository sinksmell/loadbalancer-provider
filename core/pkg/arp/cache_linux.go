/*
Copyright 2017 Caicloud authors. All rights reserved.

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

package arp

import (
	"bufio"
	"bytes"
	"fmt"
	"io/ioutil"
	"net"
	"strconv"
	"strings"
)

const (
	fIPAddr int = iota
	fHWType
	fFlags
	fHWAddr
	fDevice
	// fMask
)

const (
	cachefile = "/proc/net/arp"
)

func loadCache() (Caches, error) {
	f, err := ioutil.ReadFile(cachefile)
	if err != nil {
		return nil, err
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(bytes.NewReader(f))
	// skip first line, it is descriptions
	scanner.Scan()

	var caches = make(Caches, 0)

	for scanner.Scan() {
		c, err := parse(scanner.Text(), ifaces)
		if err != nil {
			continue
		}
		caches = append(caches, c)
	}
	return caches, nil
}

func parse(line string, ifaces []net.Interface) (*Cache, error) {
	fields := strings.Fields(line)

	ip := net.ParseIP(fields[fIPAddr])
	if ip == nil {
		return nil, fmt.Errorf("failed to parse IP addr: %v", fields[fIPAddr])
	}
	hwType, err := strconv.ParseInt(fields[fHWType], 0, 32)
	if err != nil {
		return nil, err
	}
	flag, err := strconv.ParseInt(fields[fFlags], 0, 32)
	if err != nil {
		return nil, err
	}
	hwAddr, err := net.ParseMAC(fields[fHWAddr])
	if err != nil {
		return nil, err
	}
	d := fields[fDevice]
	var dev *net.Interface
	for _, iface := range ifaces {
		if iface.Name == d {
			dev = &iface
			break
		}
	}

	return &Cache{
		IP:           ip,
		HardwareAddr: hwAddr,
		HardwareType: hwType,
		Flags:        flag,
		Interface:    dev,
	}, nil
}
