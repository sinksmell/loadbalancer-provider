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
	"fmt"
	"net"
	"os"
	"syscall"
	"text/template"
	"time"

	corenet "github.com/caicloud/loadbalancer-provider/core/pkg/net"
	"github.com/caicloud/loadbalancer-provider/pkg/execd"
	log "github.com/zoumo/logdog"

	k8sexec "k8s.io/kubernetes/pkg/util/exec"
	"k8s.io/kubernetes/pkg/util/iptables"
)

const (
	iptablesChain  = "LOADBALANCER-IPVS-DR"
	keepalivedCfg  = "/etc/keepalived/keepalived.conf"
	keepalivedTmpl = "/root/keepalived.tmpl"

	acceptMark = 1
	dropMark   = 0
	mask       = "0x00000001"
)

type ipmac struct {
	IP  string
	MAC net.HardwareAddr
}

type virtualServer struct {
	VIP        string
	Scheduler  string
	RealServer []string
}

type keepalived struct {
	useUnicast bool
	nodeIP     net.IP
	nodeInfo   *corenet.Interface
	ipt        iptables.Interface
	cmd        *execd.D
	tmpl       *template.Template
	vips       []string
}

// WriteCfg creates a new keepalived configuration file.
// In case of an error with the generation it returns the error
func (k *keepalived) UpdateConfig(vss []virtualServer, neighbors []ipmac, priority int, vrid int, httpPort int) error {
	w, err := os.Create(keepalivedCfg)
	if err != nil {
		return err
	}
	defer func() { _ = w.Close() }()
	log.Infof("Updating keealived config")
	// save vips for release when shutting down
	k.vips = getVIPs(vss)

	conf := make(map[string]interface{})
	conf["iptablesChain"] = iptablesChain
	conf["iface"] = k.nodeInfo.Name
	conf["myIP"] = k.nodeIP.String()
	conf["netmask"] = 32 // useless
	conf["vss"] = vss
	conf["vips"] = k.vips
	conf["neighbors"] = neighbors
	conf["priority"] = priority
	conf["useUnicast"] = k.useUnicast
	conf["vrid"] = vrid
	conf["acceptMark"] = acceptMark
	conf["httpPort"] = httpPort

	return k.tmpl.Execute(w, conf)
}

// getVIPs returns a list of the virtual IP addresses to be used in keepalived
// without duplicates (a service can use more than one port)
func getVIPs(svcs []virtualServer) []string {
	result := []string{}
	for _, svc := range svcs {
		result = appendIfMissing(result, svc.VIP)
	}

	return result
}

// Start starts a keepalived process in foreground.
// In case of any error it will terminate the execution with a fatal error
func (k *keepalived) Start() {
	ae, err := k.ipt.EnsureChain(iptables.TableFilter, iptables.Chain(iptablesChain))
	if err != nil {
		log.Fatalf("unexpected error: %v", err)
	}
	if ae {
		log.Infof("chain %v already existed", iptablesChain)
	}

	go k.run()
}

func (k *keepalived) isRunning() bool {
	return k.cmd.IsRunning()
}

func (k *keepalived) run() {
	k.cmd = execd.Daemon("keepalived",
		"--dont-fork",
		"--log-console",
		"--release-vips",
		"--pid", "/keepalived.pid")
	// put keepalived in another process group to prevent it
	// to receive signals meant for the controller
	// k.cmd.SysProcAttr = &syscall.SysProcAttr{
	// 	Setpgid: true,
	// 	Pgid:    0,
	// }
	k.cmd.Stdout = os.Stdout
	k.cmd.Stderr = os.Stderr

	k.cmd.SetGracePeriod(1 * time.Second)

	if err := k.cmd.RunForever(); err != nil {
		panic(fmt.Sprintf("can not run keepalived, %v", err))
	}
}

// Reload sends SIGHUP to keepalived to reload the configuration.
func (k *keepalived) Reload() error {
	log.Info("reloading keepalived")
	err := k.cmd.Signal(syscall.SIGHUP)
	if err == execd.ErrNotRunning {
		log.Warn("keepalived is not running, skip the reload")
		return nil
	}
	if err != nil {
		return fmt.Errorf("error reloading keepalived: %v", err)
	}

	return nil
}

// Stop stop keepalived process
func (k *keepalived) Stop() {
	for _, vip := range k.vips {
		_ = k.removeVIP(vip)
	}

	log.Info("flush iptables chain", log.Fields{"table": iptables.TableFilter, "chain": iptablesChain})
	err := k.ipt.FlushChain(iptables.TableFilter, iptables.Chain(iptablesChain))
	if err != nil {
		log.Errorf("unexpected error flushing iptables chain %v: %v", err, iptablesChain)
	}

	log.Info("stop keepalived process")
	err = k.cmd.Stop()
	if err != nil {
		log.Errorf("error stopping keepalived: %v", err)
	}

}

func (k *keepalived) removeVIP(vip string) error {
	log.Info("removing configured VIP %v from dev %v", vip, k.nodeInfo.Name)
	out, err := k8sexec.New().Command("ip", "addr", "del", vip+"/32", "dev", k.nodeInfo.Name).CombinedOutput()
	if err != nil {
		return fmt.Errorf("error reloading keepalived: %v\n%s", err, out)
	}
	return nil
}

func (k *keepalived) loadTemplate() error {
	tmpl, err := template.ParseFiles(keepalivedTmpl)
	if err != nil {
		return err
	}
	k.tmpl = tmpl
	return nil
}
