package ipvsdr

import (
	"bufio"
	"os"
	"time"

	"github.com/zoumo/golib/netutil"
	"github.com/zoumo/golib/shell"
	log "github.com/zoumo/logdog"
	"k8s.io/apimachinery/pkg/util/wait"
)

type ipvsCacheCleaner struct {
	vip    string
	saved  string
	stopCh chan struct{}
}

func (ipvs *ipvsCacheCleaner) start() {
	go wait.Until(ipvs.worker, 10*time.Second, ipvs.stopCh)
}

func (ipvs *ipvsCacheCleaner) stop() {
	close(ipvs.stopCh)
}

func (ipvs *ipvsCacheCleaner) worker() {
	vipExists := checkVIPExists(ipvs.vip)
	if vipExists {
		// skip check ipvs persistent connection cache
		// because it may request a lot cpu to do that if
		// there are a large number of connections
		// so we just restore it
		_ = ipvs.ipvsRestore()
		return
	}

	cacheExists := checkCacheExists()
	if cacheExists {
		// vip doesn't exist but cache exists
		// we should clean the rules and wait for cache expiring
		_ = ipvs.ipvsSaveAndClean()
	} else {
		// backup but no cache
		_ = ipvs.ipvsRestore()
	}
}

func (ipvs *ipvsCacheCleaner) ipvsSaveAndClean() error {

	if ipvs.saved != "" {
		return nil
	}

	ipvsadmCmd := shell.Command("ipvsadm").CombinedOutputClosure()
	output, err := ipvsadmCmd("-Sn")
	if err != nil {
		log.Errorf("Error save ipvs rules: %v", string(output))
		return err
	}
	if len(output) == 0 {
		// empty rules
		return nil
	}
	// clean the whole table
	msg, err := ipvsadmCmd("-C")
	if err != nil {
		log.Errorf("Error clean ipvs rules: %v", string(msg))
		return err
	}

	ipvs.saved = string(output)
	log.Info("Waiting for ipvs persistent connection hash table being empty")
	log.Infof("Saved ipvs rules %q", ipvs.saved)
	return nil
}

func (ipvs *ipvsCacheCleaner) ipvsRestore() error {
	if ipvs.saved == "" {
		return nil
	}

	msg, err := shell.Command("echo", shell.QueryEscape(ipvs.saved)).Pipe("ipvsadm", "-R").CombinedOutput()
	if err != nil {
		log.Errorf("Error restore ipvs rules: %v", string(msg))
		return err
	}

	ipvs.saved = ""
	log.Info("Restore ipvs rules")
	return nil

}

func checkVIPExists(ip string) bool {
	slice, err := netutil.InterfacesByIP(ip)
	if err != nil {
		log.Errorf("Error get net interfaces by ip", ip)
		return false
	}
	for _, iface := range slice {
		if iface.IsLoopback() {
			// skip loopback
			continue
		}
		return true
	}
	return false
}

func checkCacheExists() bool {
	// get the first 2 lines of /proc/net/ip_vs_conn
	ipvsconn, err := os.Open("/proc/net/ip_vs_conn")
	if err != nil {
		return false
	}
	defer func() { _ = ipvsconn.Close() }()

	scanner := bufio.NewScanner(ipvsconn)
	number := 0
	for scanner.Scan() {
		// the first line is header not entries
		number++
		if number >= 2 {
			return true
		}
	}
	return false
}
