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

package ingress

import (
	lbapi "github.com/caicloud/clientset/pkg/apis/loadbalance/v1alpha2"
	"github.com/caicloud/loadbalancer-provider/core/pkg/sysctl"
	core "github.com/caicloud/loadbalancer-provider/core/provider"
	"github.com/caicloud/loadbalancer-provider/pkg/version"
	log "github.com/zoumo/logdog"

	utildbus "k8s.io/kubernetes/pkg/util/dbus"
	k8sexec "k8s.io/kubernetes/pkg/util/exec"
	"k8s.io/kubernetes/pkg/util/iptables"
)

const (
//	tableRaw      = "raw"
//	iptablesChain = "INGRESS-CONTROLLER"
)

var (
	sysctlAdjustments = map[string]string{
		// about time wait connection
		// https://vincent.bernat.im/en/blog/2014-tcp-time-wait-state-linux
		// http://perthcharles.github.io/2015/08/27/timestamp-NAT/
		// deprecated: allow to reuse TIME-WAIT sockets for new connections when it is safe from protocol viewpoint
		// "net.ipv4.tcp_tw_reuse": "1",
		// deprecated: enable fast recycling of TIME-WAIT sockets
		// "net.ipv4.tcp_tw_recycle": "1",

		// about tcp keepalive
		// set tcp keepalive timeout time
		//"net.ipv4.tcp_keepalive_time": "1800",
		// set tcp keepalive probe interval
		//"net.ipv4.tcp_keepalive_intvl": "30",
		// set tcp keepalive probe times
		//"net.ipv4.tcp_keepalive_probes": "3",

		// reduse time wait buckets
		"net.ipv4.tcp_max_tw_buckets": "6000",
		// expand local port range
		//"net.ipv4.ip_local_port_range": "10240 65000",
		// reduse time to hold socket in state FIN-WAIT-2
		//"net.ipv4.tcp_fin_timeout": "30",
		// increase the maximum length of the queue for incomplete sockets
		"net.ipv4.tcp_max_syn_backlog": "8192",

		// increase the queue length for completely established sockets
		"net.core.somaxconn": "2048",
		// expand number of unprocessed input packets before kernel starts dropping them
		"net.core.netdev_max_backlog": "262144",
	}
)
var _ core.Provider = &Sidecar{}

// Sidecar ...
type Sidecar struct {
	storeLister   core.StoreLister
	ipt           iptables.Interface
	sysctlDefault map[string]string
	// tcpPorts      []string
	// udpPorts      []string
}

// NewIngressSidecar creates a new ingress sidecar
func NewIngressSidecar(lb *lbapi.LoadBalancer) (*Sidecar, error) {
	execer := k8sexec.New()
	dbus := utildbus.New()
	iptInterface := iptables.New(execer, dbus, iptables.ProtocolIpv4)

	sidecar := &Sidecar{
		sysctlDefault: make(map[string]string),
		ipt:           iptInterface,
	}

	return sidecar, nil
}

// OnUpdate ...
func (p *Sidecar) OnUpdate(lb *lbapi.LoadBalancer) error {

	return nil
}

// Start ...
func (p *Sidecar) Start() {
	log.Info("Startting ingress sidecar provider")

	_ = p.changeSysctl()
}

// WaitForStart ...
func (p *Sidecar) WaitForStart() bool {
	return true
}

// Stop ...
func (p *Sidecar) Stop() error {
	log.Info("Shutting down ingress sidecar provider")

	err := p.resetSysctl()
	if err != nil {
		log.Error("reset sysctl error", log.Fields{"err": err})
	}

	return nil
}

// Info ...
func (p *Sidecar) Info() core.Info {
	info := version.Get()
	return core.Info{
		Name:      "ingress-sidecar",
		Version:   info.Version,
		GitCommit: info.GitCommit,
		GitRemote: info.GitRemote,
	}
}

// SetListers sets the configured store listers in the generic ingress controller
func (p *Sidecar) SetListers(lister core.StoreLister) {
	p.storeLister = lister
}

// changeSysctl changes the required network setting in /proc to get
// keepalived working in the local system.
func (p *Sidecar) changeSysctl() error {
	var err error
	p.sysctlDefault, err = sysctl.BulkModify(sysctlAdjustments)
	if err != nil {
		log.Error("error change sysctl", log.Fields{"err": err})
		return err
	}
	return nil
}

// resetSysctl resets the network setting
func (p *Sidecar) resetSysctl() error {
	log.Info("reset sysctl to original value", log.Fields{"defaults": p.sysctlDefault})
	_, err := sysctl.BulkModify(p.sysctlDefault)
	return err
}
