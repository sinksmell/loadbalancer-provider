package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	log "github.com/zoumo/logdog"
	cli "gopkg.in/urfave/cli.v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/caicloud/clientset/kubernetes"
	core "github.com/caicloud/loadbalancer-provider/core/provider"
	"github.com/caicloud/loadbalancer-provider/pkg/version"
	"github.com/caicloud/loadbalancer-provider/providers/azure"
)

// Run ...
func Run(opts *Options) error {
	info := version.Get()
	log.Infof("Provider Build Information %v", info.Pretty())

	log.Info("Provider Running with", log.Fields{
		"debug":     opts.Debug,
		"kubconfig": opts.Kubeconfig,
		"lb.ns":     opts.LoadBalancerNamespace,
		"lb.name":   opts.LoadBalancerName,
		"pod.name":  opts.PodName,
		"pod.ns":    opts.PodNamespace,
	})

	if opts.Debug {
		log.ApplyOptions(log.DebugLevel)
	} else {
		log.ApplyOptions(log.InfoLevel)
	}

	// build config
	log.Infof("load kubeconfig from %s", opts.Kubeconfig)
	config, err := clientcmd.BuildConfigFromFlags("", opts.Kubeconfig)
	if err != nil {
		log.Fatal("Create kubeconfig error", log.Fields{"err": err})
		return err
	}

	// create clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatal("Create kubernetes client error", log.Fields{"err": err})
		return err
	}

	lb, err := clientset.LoadbalanceV1alpha2().LoadBalancers(opts.LoadBalancerNamespace).Get(opts.LoadBalancerName, metav1.GetOptions{})
	if err != nil {
		log.Fatal("Can not find loadbalancer resource", log.Fields{"lb.ns": opts.LoadBalancerNamespace, "lb.name": opts.LoadBalancerName})
		return err
	}

	azure, err := azure.New(clientset, opts.LoadBalancerName, opts.LoadBalancerNamespace)
	if err != nil {
		return err
	}

	lp := core.NewLoadBalancerProvider(&core.Configuration{
		KubeClient:            clientset,
		Backend:               azure,
		LoadBalancerName:      opts.LoadBalancerName,
		LoadBalancerNamespace: opts.LoadBalancerNamespace,
		TCPConfigMap:          lb.Status.ProxyStatus.TCPConfigMap,
		UDPConfigMap:          lb.Status.ProxyStatus.UDPConfigMap,
	})

	// handle shutdown
	go handleSigterm(lp)

	lp.Start()

	// never stop until sigterm processed
	<-wait.NeverStop

	return nil
}

func main() {
	flag.CommandLine.Parse([]string{})

	app := cli.NewApp()
	app.Name = "azure provider"
	app.Compiled = time.Now()
	app.Version = version.Get().Version
	// add flags to app
	opts := NewOptions()
	opts.AddFlags(app)

	app.Action = func(c *cli.Context) error {
		if err := Run(opts); err != nil {
			msg := fmt.Sprintf("running loadbalancer controller failed, with err: %v\n", err)
			return cli.NewExitError(msg, 1)
		}
		return nil
	}

	sort.Sort(cli.FlagsByName(app.Flags))

	app.Run(os.Args)
}

func handleSigterm(p *core.GenericProvider) {
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGTERM)
	<-signalChan
	log.Infof("Received SIGTERM, shutting down")

	exitCode := 0
	if err := p.Stop(); err != nil {
		log.Infof("Error during shutdown %v", err)
		exitCode = 1
	}

	log.Infof("Exiting with %v", exitCode)
	os.Exit(exitCode)
}
