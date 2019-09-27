package azure

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2018-01-01/network"
	"github.com/Azure/go-autorest/autorest/to"
	log "github.com/zoumo/logdog"
	v1 "k8s.io/api/core/v1"

	lbapi "github.com/caicloud/clientset/pkg/apis/loadbalance/v1alpha2"
	core "github.com/caicloud/loadbalancer-provider/core/provider"
	"github.com/caicloud/loadbalancer-provider/providers/azure/client"
)

const (
	// AnnotationKeyNodeMachine annotation key of machine name in node resource
	AnnotationKeyNodeMachine = "reference.caicloud.io/machine"

	azureNameFormat = "%s-%s"

	azureLoadBalancerFrontendName = "LoadBalancerFrontend"
	azureLoadBalancerBackendName  = "LoadBalancerBackend"

	azureResourceGroups    = "resourceGroups"
	azureNetworkInterfaces = "networkInterfaces"
	azureSecurityGroups    = "networkSecurityGroups"
	azureVirtualMachines   = "virtualMachines"
	azurePublicIPAddresses = "publicIPAddresses"
	azureIPConfigurations  = "/ipConfigurations"

	annotationCaicloudAzure = "caicloud-azure"
	//	annotationCaicloudAzureAKS     = "caicloud-azure-aks"
	annotationMachineCloudProvider = "machine.resource.caicloud.io/cloud-provider"
	annotationVirtualMachineID     = "machine.resource.caicloud.io/virtual-machine-id"
	labelCaicloudAKSResource       = "kubernetes.azure.com/cluster"

	// the min and max priority get from azure docs
	//	minSecurityGroupPriority             int32 = 100
	maxSecurityGroupPriority             int32 = 4096
	defaultSecurityGroupPriorityStart    int32 = 1000
	defaultSecurityGroupPriorityIncrease int32 = 10
	securityGroupTCPPrefix                     = "cps-lb-tcp-"
	securityGroupUDPPrefix                     = "cps-lb-udp-"

	azureProviderStatusFormat                   = `{"status":{"providersStatuses":{"azure":{"phase":"%s","reason":"%s","message":"%s", "provisioningState":"%s"}}}}`
	azureProviderStatusAndPublicIPAddressFormat = `{"status":{"providersStatuses":{"azure":{"phase":"%s","reason":"%s","message":"%s", "provisioningState":"%s", "publicIPAddress":"%s"}}}}`

	azureFinalizer = "finalizer.azure.loadbalancer.loadbalance.caicloud.io"
)

// MachineInfo machine info
type machineInfo struct {
	VMID      string
	VMName    string
	GroupName string
}

func (m *machineInfo) Parse() (err error) {
	if len(m.VMName) == 0 {
		m.VMName, err = getSpecifyName(m.VMID, azureVirtualMachines)
		if err != nil {
			return err
		}
	}
	if len(m.GroupName) == 0 {
		m.GroupName, err = getSpecifyName(m.VMID, azureResourceGroups)
		return err
	}
	return nil
}

type networkInterfaceIDSet map[string]struct{}
type securityGroupIDSet map[string]struct{}

func getAzureNetworkInterfacesByNodeName(c *client.Client, nodeName string, storeLister *core.StoreLister) ([]string, error) {
	machine, err := getMachineInfoFromNode(c, nodeName, storeLister)
	if err != nil {
		return nil, err
	}
	return getNetworkInterfacesFromVM(c, machine)
}

func getMachineInfoFromNode(c *client.Client, nodeName string, storeLister *core.StoreLister) (*machineInfo, error) {
	node, err := storeLister.Node.Get(nodeName)
	if err != nil {
		log.Errorf("get node %s falied", nodeName)
		return nil, err
	}
	cloudProvider, ok := node.Annotations[annotationMachineCloudProvider]
	if !ok {
		return getMachineInfoInAKSCluster(c, node)
	}
	if cloudProvider != annotationCaicloudAzure {
		return nil, fmt.Errorf(" azure node type %s error", cloudProvider)
	}
	vmID, ok := node.Annotations[annotationVirtualMachineID]
	if !ok {
		return nil, fmt.Errorf("get node virtual machine ID failed")
	}
	return &machineInfo{VMID: vmID}, nil
}

func getMachineInfoInAKSCluster(c *client.Client, node *v1.Node) (*machineInfo, error) {
	_ = c
	resourceGroup, ok := node.Labels[labelCaicloudAKSResource]
	if !ok {
		return nil, fmt.Errorf("get %s label failed", labelCaicloudAKSResource)
	}
	return &machineInfo{
		VMName:    node.Name,
		GroupName: resourceGroup,
	}, nil
}

func getNetworkInterfacesFromVM(c *client.Client, machine *machineInfo) ([]string, error) {
	if machine == nil {
		return nil, fmt.Errorf("invalid vm info")
	}
	err := machine.Parse()
	if err != nil {
		return nil, err
	}
	vm, err := c.VM.Get(context.TODO(), machine.GroupName, machine.VMName, "")
	if err != nil {
		return nil, err
	}

	if vm.NetworkProfile.NetworkInterfaces == nil || len(*vm.NetworkProfile.NetworkInterfaces) == 0 {
		return nil, fmt.Errorf("vm %s has no networkInterfaces", to.String(vm.Name))
	}
	networkInterfaces := make([]string, 0, len(*vm.NetworkProfile.NetworkInterfaces))
	for _, networkInterface := range *vm.NetworkProfile.NetworkInterfaces {
		networkInterfaces = append(networkInterfaces, to.String(networkInterface.ID))
	}
	return networkInterfaces, nil
}

func getAzureLBName(lbName string, clusterID string) string {
	return fmt.Sprintf(azureNameFormat, lbName, clusterID)
}

func defaultClusterAzureLBProbes() []network.Probe {
	// default tcp probe port 80, 443
	// rule default use 80 probe
	m := []int32{80, 443}
	re := make([]network.Probe, 0, len(m))
	for _, port := range m {
		probeName := strconv.Itoa(int(port))
		re = append(re, network.Probe{
			Name: to.StringPtr(probeName),
			ProbePropertiesFormat: &network.ProbePropertiesFormat{
				Protocol:          network.ProbeProtocolTCP,
				Port:              to.Int32Ptr(port),
				IntervalInSeconds: to.Int32Ptr(5),
				NumberOfProbes:    to.Int32Ptr(3),
			},
		})
	}
	return re
}

func azureLBProbes2Rules(frontendIPConfigurationID, backendAddressPoolID string, probes []network.Probe) []network.LoadBalancingRule {
	re := make([]network.LoadBalancingRule, 0, len(probes))
	for i := range probes {
		probe := &probes[i]
		ruleName := getRuleName(to.Int32(probe.Port), network.TransportProtocolTCP)
		re = append(re, network.LoadBalancingRule{
			Name: to.StringPtr(ruleName),
			LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{
				FrontendIPConfiguration: &network.SubResource{
					ID: to.StringPtr(frontendIPConfigurationID),
				},
				BackendAddressPool: &network.SubResource{
					ID: to.StringPtr(backendAddressPoolID),
				},
				Probe: &network.SubResource{
					ID: probe.ID,
				},
				Protocol: network.TransportProtocolTCP,
				// The load distribution policy for this rule
				// make sure the traffic distributed to one backend in a conversation
				LoadDistribution:     network.SourceIPProtocol,
				FrontendPort:         probe.Port,
				BackendPort:          probe.Port,
				IdleTimeoutInMinutes: to.Int32Ptr(4),
				EnableFloatingIP:     to.BoolPtr(false),
				DisableOutboundSnat:  to.BoolPtr(false),
			},
		})
	}
	return re
}

func getGroupName(lb *lbapi.LoadBalancer) string {
	azure := lb.Spec.Providers.Azure
	if azure == nil {
		return ""
	}
	return azure.ResourceGroupName
}

func isDefaultRules(rule *network.LoadBalancingRule) bool {
	if rule.Protocol != network.TransportProtocolTCP {
		return false
	}
	port := to.Int32(rule.FrontendPort)
	if port != 80 && port != 443 {
		return false
	}
	return true
}

func getRuleName(port int32, protocol network.TransportProtocol) string {
	name := fmt.Sprintf("%s-%d", protocol, port)
	return strings.ToLower(name)
}

//return true if azlb has diff with tcp and udp map
func makeUpRules(azlb *network.LoadBalancer, tcpMap, udpMap map[string]string) bool {

	var oldRules []network.LoadBalancingRule
	if azlb.LoadBalancingRules == nil || len(*azlb.LoadBalancingRules) == 0 {
		oldRules = make([]network.LoadBalancingRule, 0, len(tcpMap)+len(udpMap))
	} else {
		oldRules = *(azlb.LoadBalancingRules)
	}

	// remain the Constant rule
	newRules, diff := remainConstantRules(oldRules, tcpMap, udpMap)
	if len(tcpMap) != 0 || len(udpMap) != 0 {
		diff = true
	}
	// add new tcp rule
	for port, service := range tcpMap {
		newRule, err := newRuleWithConfig(azlb, port, service, network.TransportProtocolTCP)
		if err != nil {
			log.Warnf("invalid port %v error : %v", port, err)
			continue
		}
		newRules = append(newRules, *newRule)
	}
	// add new udp rule
	for port, service := range udpMap {
		newRule, err := newRuleWithConfig(azlb, port, service, network.TransportProtocolUDP)
		if err != nil {
			log.Warnf("invalid port %v error : %v", port, err)
			continue
		}
		newRules = append(newRules, *newRule)
	}

	if diff {
		azlb.LoadBalancingRules = &newRules
	}
	return diff
}

// remain constant rules, will delete the constant rule from tcpMap and udpMap
func remainConstantRules(oldRules []network.LoadBalancingRule, tcpMap, udpMap map[string]string) ([]network.LoadBalancingRule, bool) {
	newRules := make([]network.LoadBalancingRule, 0, len(oldRules))
	var diff bool
	for _, rule := range oldRules {
		port := to.Int32(rule.FrontendPort)
		portKey := strconv.FormatInt(int64(port), 10)
		var m map[string]string
		switch rule.Protocol {
		case network.TransportProtocolTCP:
			m = tcpMap
		case network.TransportProtocolUDP:
			m = udpMap
		default:
			log.Warnf("invalid Protocol %s ", rule.Protocol)
			newRules = append(newRules, rule)
			continue
		}
		_, ok := m[portKey]
		if ok {
			ruleName := getRuleName(port, rule.Protocol)
			if to.String(rule.Name) == ruleName {
				delete(m, portKey)
				// fmt.Printf("ruleName %s rule.Name %s\n", ruleName, to.String(rule.Name))
				newRules = append(newRules, rule)
				continue
			}
		}
		if isDefaultRules(&rule) {
			newRules = append(newRules, rule)
			continue
		}
		diff = true
	}
	return newRules, diff
}

//new rule
func newRuleWithConfig(azlb *network.LoadBalancer, port, service string, protocol network.TransportProtocol) (*network.LoadBalancingRule, error) {
	_ = service
	// check
	if azlb.Probes == nil || len(*azlb.Probes) == 0 {
		return nil, fmt.Errorf("azure lb probes is nil")
	}
	if azlb.FrontendIPConfigurations == nil || len(*azlb.FrontendIPConfigurations) == 0 {
		return nil, fmt.Errorf("azure lb frontendIPConfigurations is nil")
	}
	if azlb.BackendAddressPools == nil || len(*azlb.BackendAddressPools) == 0 {
		return nil, fmt.Errorf("azure lb backendAddressPools is nil")
	}
	// get param
	probeID := (*azlb.Probes)[0].ID
	frontendIPConfigurationID := to.String((*azlb.FrontendIPConfigurations)[0].ID)
	backendAddressPoolID := to.String((*azlb.BackendAddressPools)[0].ID)

	lbPort, err := strconv.Atoi(port)
	if err != nil {
		return nil, err
	}
	ruleName := getRuleName(int32(lbPort), protocol)

	rule := &network.LoadBalancingRule{
		Name: to.StringPtr(ruleName),
		LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{
			FrontendIPConfiguration: &network.SubResource{
				ID: to.StringPtr(frontendIPConfigurationID),
			},
			BackendAddressPool: &network.SubResource{
				ID: to.StringPtr(backendAddressPoolID),
			},
			Probe: &network.SubResource{
				ID: probeID,
			},
			Protocol: protocol,
			// The load distribution policy for this rule,
			// make sure the traffic distributed to one backend in a conversation
			LoadDistribution:     network.SourceIPProtocol,
			FrontendPort:         to.Int32Ptr(int32(lbPort)),
			BackendPort:          to.Int32Ptr(int32(lbPort)),
			IdleTimeoutInMinutes: to.Int32Ptr(4),
			EnableFloatingIP:     to.BoolPtr(false),
			DisableOutboundSnat:  to.BoolPtr(false),
		},
	}
	return rule, nil
}

func getGroupAndResourceNameFromID(id, resourceType string) (string, string, error) {
	groupName, err := getSpecifyName(id, azureResourceGroups)
	if err != nil {
		return "", "", err
	}
	name, err := getSpecifyName(id, resourceType)
	if err != nil {
		return "", "", err
	}
	return groupName, name, nil
}

func getSpecifyName(id, prefix string) (string, error) {
	index := strings.Index(id, prefix)
	if index == -1 {
		log.Errorf("find id %s  prefix %s failed", id, prefix)
		return "", fmt.Errorf("find prefix %s failed", prefix)
	}
	reSlice := strings.Split(id[index:], "/")
	if len(reSlice) < 2 {
		log.Errorf("find id %s  prefix %s failed", id, prefix)
		return "", fmt.Errorf("find prefix %s failed", prefix)
	}
	return reSlice[1], nil
}

func diffBackendPoolNetworkInterfaecs(c *client.Client, azlb *network.LoadBalancer, nodes []string, storeLister *core.StoreLister) ([]string, []string, networkInterfaceIDSet, error) {
	// get source data
	azBackendPoolMap, err := getAzureBackendPoolIP(azlb)
	if err != nil {
		return nil, nil, nil, err
	}
	cpsLBBackendConfigMap, err := getBackendPoolIPFromNodes(c, nodes, storeLister)
	if err != nil {
		return nil, nil, nil, err
	}

	log.Infof("az backend %v new backend %v\n", azBackendPoolMap, cpsLBBackendConfigMap)
	// get diff data
	detachNetworkInterfaces := getDiffBetweenNetworkInterfaces(azBackendPoolMap, cpsLBBackendConfigMap)
	attachNetworkInterfaces := getDiffBetweenNetworkInterfaces(cpsLBBackendConfigMap, azBackendPoolMap)

	return detachNetworkInterfaces, attachNetworkInterfaces, cpsLBBackendConfigMap, nil
}

// getDiffBetweenNetworkInterfaces find the key in one but not in two
func getDiffBetweenNetworkInterfaces(one, two networkInterfaceIDSet) []string {
	diff := make([]string, 0, len(one))
	for id := range one {
		_, ok := two[id]
		if !ok {
			diff = append(diff, id)
		}
	}
	return diff
}

func getAzureBackendPoolIP(azlb *network.LoadBalancer) (networkInterfaceIDSet, error) {
	// check
	if azlb.BackendAddressPools == nil || len(*azlb.BackendAddressPools) == 0 {
		return nil, fmt.Errorf("azure lb backendAddressPools is nil")
	}
	azBackendPool := (*azlb.BackendAddressPools)[0]
	azBackendPoolIPIDMap := make(networkInterfaceIDSet)
	if azBackendPool.BackendIPConfigurations == nil || len(*azBackendPool.BackendIPConfigurations) == 0 {
		return azBackendPoolIPIDMap, nil
	}
	for _, config := range *azBackendPool.BackendIPConfigurations {
		id := to.String(config.ID)
		ipConfigID, err := getIPIDFromIPConfig(id)
		if err != nil {
			log.Warnf("get networkinterface failed")
			continue
		}
		azBackendPoolIPIDMap[ipConfigID] = struct{}{}
	}
	return azBackendPoolIPIDMap, nil
}

func getBackendPoolIPFromNodes(c *client.Client, nodes []string, storeLister *core.StoreLister) (networkInterfaceIDSet, error) {
	log.Infof("new backend pools nodes %v", nodes)
	set := make(networkInterfaceIDSet)
	for _, node := range nodes {
		nets, err := getAzureNetworkInterfacesByNodeName(c, node, storeLister)
		if err != nil {
			return nil, err
		}
		for _, net := range nets {
			set[net] = struct{}{}
		}
	}
	return set, nil
}

func getIPIDFromIPConfig(id string) (string, error) {
	index := strings.Index(id, azureIPConfigurations)
	if index == -1 {
		return "", fmt.Errorf("find ipConfigurations failed")
	}
	return id[:index], nil
}

func copyMap(m map[string]string) map[string]string {
	newm := make(map[string]string)
	for k, v := range m {
		newm[k] = v
	}
	return newm
}
