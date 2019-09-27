package azure

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2018-01-01/network"
	"github.com/Azure/go-autorest/autorest"
	"github.com/Azure/go-autorest/autorest/to"
	log "github.com/zoumo/logdog"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/caicloud/clientset/kubernetes"
	lbapi "github.com/caicloud/clientset/pkg/apis/loadbalance/v1alpha2"
	core "github.com/caicloud/loadbalancer-provider/core/provider"
	"github.com/caicloud/loadbalancer-provider/pkg/version"
	"github.com/caicloud/loadbalancer-provider/providers/azure/client"
)

// Provider azure lb provider
type Provider struct {
	storeLister           core.StoreLister
	clientset             *kubernetes.Clientset
	loadBalancerNamespace string
	loadBalancerName      string

	// old azure lb azure spec
	oldAzureProvider *lbapi.AzureProvider
	nodes            []string

	// load balancer rules cache
	tcpRuleMap map[string]string
	udpRuleMap map[string]string

	// cleanAzure
	cleanAzure bool
}

// New creates a new azure LoadBalancer Provider.
func New(clientset *kubernetes.Clientset, name, namespace string) (*Provider, error) {
	azure := &Provider{
		clientset:             clientset,
		loadBalancerName:      name,
		loadBalancerNamespace: namespace,
	}
	return azure, nil
}

// SetListers set store lister
func (l *Provider) SetListers(storeLister core.StoreLister) {
	l.storeLister = storeLister
}

func (l *Provider) setCacheAzureLoadbalancer(azure *lbapi.AzureProvider) {
	if l.oldAzureProvider == nil {
		l.oldAzureProvider = &lbapi.AzureProvider{}
	}
	l.oldAzureProvider.Name = azure.Name
	l.oldAzureProvider.ResourceGroupName = azure.ResourceGroupName
}

func (l *Provider) setCacheReserveStatus(reserve *bool) {
	if l.oldAzureProvider == nil {
		l.oldAzureProvider = &lbapi.AzureProvider{}
	}
	log.Infof("set reserve status %v .", to.Bool(reserve))
	l.oldAzureProvider.ReserveAzure = reserve
}

// func hasAzureFinalizer(lb *lbapi.LoadBalancer) bool {
// 	for _, v := range lb.Finalizers {
// 		if v == azureFinalizer {
// 			return true
// 		}
// 	}
// 	return false
// }

// OnUpdate update loadbalancer
func (l *Provider) OnUpdate(lb *lbapi.LoadBalancer) error {

	log.Infof("OnUpdate......")
	if lb.Spec.Providers.Azure == nil {
		return l.cleanupAzureLB(nil, false)
	}

	if lb.DeletionTimestamp != nil {
		return l.cleanupAzureLB(lb, true)
	}

	// ignore change of azure's name groupName and reserve status
	l.setCacheAzureLoadbalancer(lb.Spec.Providers.Azure)
	l.setCacheReserveStatus(lb.Spec.Providers.Azure.ReserveAzure)

	// tell if change of load balancer
	tcp, udp, ruleChange, err := l.getProxyConfigMapAndCompare(lb)
	if err != nil {
		return err
	}
	// ignore change of other providers
	if reflect.DeepEqual(lb.Spec.Providers.Azure, l.oldAzureProvider) &&
		reflect.DeepEqual(l.nodes, lb.Spec.Nodes.Names) &&
		!ruleChange {
		return nil
	}

	// check lb exist in cluster
	_, err = l.storeLister.LoadBalancer.LoadBalancers(lb.Namespace).Get(lb.Name)
	if errors.IsNotFound(err) {
		return l.cleanupAzureLB(lb, true)
	}
	if err != nil {
		log.Errorf("get lb falied %v", err)
		return err
	}

	nlb := lb.DeepCopy()

	azlb, ip, err := l.ensureSync(nlb, tcp, udp)

	l.updateLoadBalancerAzureStatus(azlb, lb, ip, err)
	if err == nil {
		log.Infof("update cache data %v tcp %v udp %v", nlb.Spec.Providers.Azure, tcp, udp)
		l.updateCacheData(nlb, tcp, udp)
	}
	return err
}

func (l *Provider) updateCacheData(lb *lbapi.LoadBalancer, tcp, udp map[string]string) {
	l.oldAzureProvider = lb.Spec.Providers.Azure
	l.nodes = lb.Spec.Nodes.Names
	l.tcpRuleMap = tcp
	l.udpRuleMap = udp
}

// Start ...
func (l *Provider) Start() {
	log.Infof("Startting azure provider ns %s name %s", l.loadBalancerNamespace, l.loadBalancerName)
}

// Stop ...
func (l *Provider) Stop() error {
	log.Infof("end provider azure...")
	_, err := l.storeLister.LoadBalancer.LoadBalancers(l.loadBalancerNamespace).Get(l.loadBalancerName)
	if errors.IsNotFound(err) {
		return l.cleanupAzureLB(nil, true)
	}
	return nil
}

// WaitForStart waits for
func (l *Provider) WaitForStart() bool {
	// err := wait.Poll(time.Second, 60*time.Second, func() (bool, error) {
	// 	//
	// 	return true, nil
	// })

	// if err != nil {
	// 	return false
	// }
	return true
}

// Info information about the provider.
func (l *Provider) Info() core.Info {
	info := version.Get()
	return core.Info{
		Name:      "azure",
		Version:   info.Version,
		GitCommit: info.GitCommit,
		GitRemote: info.GitRemote,
	}
}

func (l *Provider) updateLoadBalancerAzureStatus(azlb *network.LoadBalancer, lb *lbapi.LoadBalancer, publicIPAddress string, err error) {
	if azlb != nil {
		setProvisioningState(lb, to.String(azlb.ProvisioningState))
	}
	setProvisioningPublicIPAddress(lb, publicIPAddress)
	if err == nil {
		_, _ = l.patchLoadBalancerAzureStatus(lb, lbapi.AzureRunningPhase, nil)
	} else {
		_, _ = l.patchLoadBalancerAzureStatus(lb, lbapi.AzureErrorPhase, err)
	}
}

// make sure azure lb config stay in same with compass lb
func (l *Provider) ensureSync(lb *lbapi.LoadBalancer, tcp, udp map[string]string) (*network.LoadBalancer, string, error) {

	azureSpec := lb.Spec.Providers.Azure
	log.Infof("start sync azlb group %s name %s tcp %v", azureSpec.ResourceGroupName, azureSpec.Name, tcp)

	// update status
	_, err := l.patchLoadBalancerAzureStatus(lb, lbapi.AzureUpdatingPhase, nil)
	if err != nil {
		return nil, "", err
	}

	c, err := client.NewClient(&l.storeLister)
	if err != nil {
		log.Errorf("init client error %v", err)
		return nil, "", err
	}

	// get a valid azure load balancer
	azlb, err := l.ensureAzureLoadbalancer(c, lb)
	if err != nil {
		return nil, "", err
	}

	// make sure default config is correct
	azlb, err = ensureSyncDefaultAzureLBConfig(c, azlb, lb)

	if err != nil {
		return nil, "", err
	}

	ip, err := getPublicIPAddress(c, lb)
	if err != nil {
		return nil, "", err
	}

	err = ensureSyncRulesAndBackendPools(c, &l.storeLister, azlb, lb, tcp, udp)

	return azlb, ip, err
}

func getPublicIPAddress(c *client.Client, lb *lbapi.LoadBalancer) (string, error) {
	if lb != nil && lb.Spec.Providers.Azure != nil &&
		lb.Spec.Providers.Azure.IPAddressProperties.Public != nil {
		public := lb.Spec.Providers.Azure.IPAddressProperties.Public
		group, name, err := getGroupAndResourceNameFromID(to.String(public.PublicIPAddressID), azurePublicIPAddresses)
		if err != nil {
			return "", err
		}
		address, err := c.PublicIPAddress.Get(context.TODO(), group, name, "")
		if err != nil {
			return "", err
		}
		return to.String(address.IPAddress), nil
	}
	return "", nil
}

// get compass lb proxy info and compare with cache data
func (l *Provider) getProxyConfigMapAndCompare(lb *lbapi.LoadBalancer) (map[string]string, map[string]string, bool, error) {
	tcpCm, err := l.storeLister.ConfigMap.ConfigMaps(lb.Namespace).Get(lb.Status.ProxyStatus.TCPConfigMap)
	if err != nil {
		log.Errorf("get namespace %s cm %s failed err : %v", lb.Namespace, lb.Status.ProxyStatus.TCPConfigMap, err)
		return nil, nil, false, client.NewServiceError("K8SStore", err.Error())
	}
	udpCm, err := l.storeLister.ConfigMap.ConfigMaps(lb.Namespace).Get(lb.Status.ProxyStatus.UDPConfigMap)
	if err != nil {
		log.Errorf("get namespace %s cm %s failed err : %v", lb.Namespace, lb.Status.ProxyStatus.TCPConfigMap, err)
		return nil, nil, false, client.NewServiceError("K8SStore", err.Error())
	}
	if !reflect.DeepEqual(l.tcpRuleMap, tcpCm.Data) || !reflect.DeepEqual(l.udpRuleMap, udpCm.Data) {
		return tcpCm.Data, udpCm.Data, true, nil
	}
	return tcpCm.Data, udpCm.Data, false, nil
}

// get a valid azure load balancer
func (l *Provider) ensureAzureLoadbalancer(c *client.Client, lb *lbapi.LoadBalancer) (*network.LoadBalancer, error) {
	azureSpec := lb.Spec.Providers.Azure
	azlb, err := getAzureLoadbalancer(c, azureSpec.ResourceGroupName, azureSpec.Name)
	if err != nil {
		return nil, err
	}
	if azlb == nil {
		azlb, err = createAzureLoadBalancer(c, lb)
		if err != nil {
			return nil, err
		}
		if len(azureSpec.Name) == 0 {
			azureSpec.Name = to.String(azlb.Name)
			err = l.pathLoadBalancerName(lb, azureSpec.Name)
			if err != nil {
				return nil, err
			}
		}
	}
	return azlb, nil
}

func (l *Provider) pathLoadBalancerName(lb *lbapi.LoadBalancer, name string) error {
	lb.Spec.Providers.Azure.Name = name
	l.setCacheAzureLoadbalancer(lb.Spec.Providers.Azure)
	patch := fmt.Sprintf(`{"spec":{"providers":{"azure":{"name":"%s"}}}}`, name)
	_, err := l.clientset.LoadbalanceV1alpha2().LoadBalancers(lb.Namespace).Patch(lb.Name, types.MergePatchType, []byte(patch))
	if err != nil {
		log.Errorf("patch lb %s failed %v", lb.Name, err)
		return err
	}
	return nil
}

func (l *Provider) getFinalizersPatch(lb *lbapi.LoadBalancer) string {
	if lb == nil {
		return ""
	}
	remainFinalizes := make([]string, 0, len(lb.Finalizers))
	for i := range lb.Finalizers {
		if lb.Finalizers[i] != azureFinalizer {
			remainFinalizes = append(remainFinalizes, fmt.Sprintf("%q", lb.Finalizers[i]))
		}
	}
	if len(remainFinalizes) == len(lb.Finalizers) {
		return ""
	}
	return fmt.Sprintf(`"metadata":{"finalizers":[%s]}`, strings.Join(remainFinalizes, ","))
}

func (l *Provider) getNilAzureStatus() string {
	return `"status":{"ProvidersStatuses":{"azure":{}}}`
}

func (l *Provider) patachFinalizersAndStatus(lb *lbapi.LoadBalancer, deleteLB bool) error {
	patchs := make([]string, 0, 2)
	finalizers := l.getFinalizersPatch(lb)
	if len(finalizers) != 0 {
		patchs = append(patchs, finalizers)
	}
	if !deleteLB {
		patchs = append(patchs, l.getNilAzureStatus())
	}

	if len(patchs) == 0 {
		return nil
	}

	namespace, name := l.loadBalancerNamespace, l.loadBalancerName
	if lb != nil {
		namespace, name = lb.Namespace, lb.Name
	}

	patchJSON := strings.Join(patchs, ",")
	patchJSON = fmt.Sprintf("{%s}", patchJSON)
	_, err := l.clientset.LoadbalanceV1alpha2().LoadBalancers(namespace).Patch(name, types.MergePatchType, []byte(patchJSON))
	if err != nil {
		log.Errorf("patch lb finalizers %s failed %v patch info %s", name, err, patchJSON)
		return err
	}
	return nil
}

// patch load balancer azure status
func (l *Provider) patchLoadBalancerAzureStatus(lb *lbapi.LoadBalancer, phase lbapi.AzureProviderPhase, result error) (*lbapi.LoadBalancer, error) {
	var reason, message string
	var serviceError *client.ServiceError
	switch t := result.(type) {
	case autorest.DetailedError:
		serviceError = client.ParseServiceError(result)
		if serviceError != nil {
			reason = serviceError.Code
			message = serviceError.Message
		} else {
			reason = "Unknown"
			message = result.Error()
		}
	case *client.ServiceError:
		reason = t.Code
		message = t.Message
	default:
		if result != nil {
			reason = "Unknown"
			message = result.Error()
		}
	}

	var provisioningState string
	var publicIPAddress string
	if lb != nil && lb.Status.ProvidersStatuses.Azure != nil {
		provisioningState = lb.Status.ProvidersStatuses.Azure.ProvisioningState
		publicIPAddress = to.String(lb.Status.ProvidersStatuses.Azure.PublicIPAddress)
	}
	var patch string
	if result == nil {
		patch = fmt.Sprintf(azureProviderStatusAndPublicIPAddressFormat, phase, reason, message, provisioningState, publicIPAddress)
	} else {
		patch = fmt.Sprintf(azureProviderStatusFormat, phase, reason, message, provisioningState)
	}

	namespace, name := l.loadBalancerNamespace, l.loadBalancerName
	if lb != nil {
		namespace, name = lb.Namespace, lb.Name
	}
	lb, err := l.clientset.LoadbalanceV1alpha2().LoadBalancers(namespace).Patch(name, types.MergePatchType, []byte(patch))
	if err != nil {
		log.Errorf("patch lb %s failed %v", name, err)
		return nil, err
	}
	return lb, nil
}

// clean up azure lb info and make oldAzureProvider nil
func (l *Provider) cleanupAzureLB(lb *lbapi.LoadBalancer, deleteLB bool) error {
	log.Info("start clean up ")
	if l.cleanAzure {
		log.Info("azure loadbalancer is already clean...")
		return nil
	}

	if l.oldAzureProvider == nil || len(l.oldAzureProvider.Name) == 0 {
		log.Errorf("old azure info nil")
		err := l.patachFinalizersAndStatus(lb, deleteLB)
		if err == nil {
			l.cleanAzure = true
		}
		return err
	}
	c, err := client.NewClient(&l.storeLister)
	if err != nil {
		log.Errorf("init client error %v", err)
		return err
	}

	defer func() {
		if err == nil {
			l.oldAzureProvider = nil
		}
	}()

	err = cleanUpSecurityGroup(c, l.oldAzureProvider.ResourceGroupName, l.oldAzureProvider.Name)
	if err != nil {
		return err
	}

	reserve := to.Bool(l.oldAzureProvider.ReserveAzure)
	if lb != nil && lb.Spec.Providers.Azure != nil {
		reserve = to.Bool(lb.Spec.Providers.Azure.ReserveAzure)
	}

	if reserve {
		err = wait.Poll(5*time.Second, 60*time.Second, func() (bool, error) {
			err = recoverDefaultAzureLoadBalancer(c, l.oldAzureProvider.ResourceGroupName, l.oldAzureProvider.Name)
			if err == nil {
				return true, nil
			}
			return false, nil
		})
		if err != nil {
			_, _ = l.patchLoadBalancerAzureStatus(lb, lbapi.AzureErrorPhase, err)
			return err
		}
		err = l.patachFinalizersAndStatus(lb, deleteLB)
		if err == nil {
			l.cleanAzure = true
		}
		return err
	}
	log.Infof("delete azure lb group %s name %s", l.oldAzureProvider.ResourceGroupName, l.oldAzureProvider.Name)
	err = c.LoadBalancer.Delete(context.TODO(), l.oldAzureProvider.ResourceGroupName, l.oldAzureProvider.Name)
	log.Infof("delete result %v", err)
	if err != nil {
		return err
	}
	err = l.patachFinalizersAndStatus(lb, deleteLB)
	if err == nil {
		l.cleanAzure = true
	}
	return err
}

func cleanUpSecurityGroup(c *client.Client, groupName, lbName string) error {
	log.Info("start cleanUpSecurityGroup...")
	// get azure lb
	azlb, err := c.LoadBalancer.Get(context.TODO(), groupName, lbName, "")
	// this can be happened to be deleted by others, ignore it
	if client.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	// get networks
	azBackendPoolMap, _ := getAzureBackendPoolIP(&azlb)
	detachs := getDiffBetweenNetworkInterfaces(azBackendPoolMap, nil)
	// get the sg to be delete
	deleteSg, _, err := getSuitableSecurityGroup(c, detachs, nil)
	if err != nil {
		return err
	}
	// delete the useless rules from security group
	return deleteUselessSecurityRules(c, deleteSg)
}
