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

package ipvsdr

import (
	"crypto/md5"
	"encoding/hex"
	"io"
	"os"
)

var (
//	invalidIfaces = []string{"lo", "docker0", "flannel.1", "cbr0"}
//	nsSvcLbRegex  = regexp.MustCompile(`(.*)/(.*):(.*)|(.*)/(.*)`)
//	vethRegex     = regexp.MustCompile(`^veth.*`)
//	lvsRegex      = regexp.MustCompile(`NAT`)
)

type stringSlice []string

// pos returns the position of a string in a slice.
// If it does not exists in the slice returns -1.
func (slice stringSlice) pos(value string) int {
	for p, v := range slice {
		if v == value {
			return p
		}
	}

	return -1
}

func appendIfMissing(slice []string, item string) []string {
	for _, elem := range slice {
		if elem == item {
			return slice
		}
	}
	return append(slice, item)
}

func getNeighbors(ip string, nodes []string) (neighbors []string) {
	for _, neighbor := range nodes {
		if ip != neighbor {
			neighbors = append(neighbors, neighbor)
		}
	}
	return
}

// getPriority returns the priority of one node using the
// IP address as key. It starts in 100
func getNodePriority(ip string, nodes []string) int {
	return 100 + stringSlice(nodes).pos(ip)
}

func checksum(filename string) (string, error) {
	var result []byte
	file, err := os.Open(filename)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()

	hash := md5.New()
	_, err = io.Copy(hash, file)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(result)), nil
}
