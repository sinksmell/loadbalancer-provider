package azure

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2018-01-01/network"
	"github.com/Azure/go-autorest/autorest/to"
	log "github.com/zoumo/logdog"

	lbapi "github.com/caicloud/clientset/pkg/apis/loadbalance/v1alpha2"
	core "github.com/caicloud/loadbalancer-provider/core/provider"
	"github.com/caicloud/loadbalancer-provider/providers/azure/client"
)

// it will delete default info when recover default azlb
// make sure config the correct default info
func ensureSyncDefaultAzureLBConfig(c *client.Client, azlb *network.LoadBalancer, lb *lbapi.LoadBalancer) (*network.LoadBalancer, error) {
	var err error
	azlb, change, err := ensureSyncDefaultConfigExceptRules(azlb, lb)
	if err != nil {
		return nil, err
	}
	if change {
		// if probe frontend or backend changed, must clean rules because of rules will invoke
		// the resources reference above all, but the resources can be deleted
		if *azlb.LoadBalancingRules != nil && len(*azlb.LoadBalancingRules) != 0 {
			*azlb.LoadBalancingRules = (*azlb.LoadBalancingRules)[:0]
		}
		*azlb, err = c.LoadBalancer.CreateOrUpdate(context.TODO(), lb.Spec.Providers.Azure.ResourceGroupName, to.String(azlb.Name), *azlb)
		if err != nil {
			log.Errorf("update lb failed error : %v", err)
			return nil, err
		}
	}
	azlb, err = ensureSyncDefaultRules(c, azlb, lb)
	if err != nil {
		return nil, err
	}
	return azlb, nil
}

// getProbesBrief: get brief info from azure api
func getProbesBrief(probes *[]network.Probe) *[]network.Probe {
	if probes == nil || len(*probes) == 0 {
		return nil
	}
	brief := make([]network.Probe, 0, len(*probes))
	for _, probe := range *probes {
		brief = append(brief, network.Probe{
			Name: probe.Name,
			ProbePropertiesFormat: &network.ProbePropertiesFormat{
				Protocol:          probe.Protocol,
				Port:              probe.Port,
				IntervalInSeconds: probe.IntervalInSeconds,
				NumberOfProbes:    probe.NumberOfProbes,
			},
		})
	}
	return &brief
}

// get Azure LoadBalancer FrontendIPConfig from properties
func getAzureLoadBalancerFrontendIPConfigByConfig(properties lbapi.AzureIPAddressProperties) *[]network.FrontendIPConfiguration {
	frontendIPConfigurations := make([]network.FrontendIPConfiguration, 1)
	frontendIPConfiguration := network.FrontendIPConfiguration{
		Name:                                    to.StringPtr(azureLoadBalancerFrontendName),
		FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{},
	}
	// make up front end private address config
	if properties.Private != nil {
		private := properties.Private
		frontendIPConfiguration.FrontendIPConfigurationPropertiesFormat.PrivateIPAllocationMethod = network.IPAllocationMethod(private.IPAllocationMethod)
		frontendIPConfiguration.FrontendIPConfigurationPropertiesFormat.PrivateIPAddress = private.PrivateIPAddress
		frontendIPConfiguration.FrontendIPConfigurationPropertiesFormat.Subnet = &network.Subnet{
			ID: to.StringPtr(private.SubnetID),
		}
	}
	// make up front end public address config
	if properties.Public != nil {
		public := properties.Public
		frontendIPConfiguration.FrontendIPConfigurationPropertiesFormat.PublicIPAddress = &network.PublicIPAddress{
			ID: public.PublicIPAddressID,
		}
	}
	frontendIPConfigurations[0] = frontendIPConfiguration
	return &frontendIPConfigurations
}

func getAzureLoadBalancerFrontendIPConfigBrief(ipConfigs *[]network.FrontendIPConfiguration) *[]network.FrontendIPConfiguration {
	if ipConfigs == nil || len(*ipConfigs) == 0 {
		return nil
	}
	briefs := make([]network.FrontendIPConfiguration, 0, len(*ipConfigs))
	for _, config := range *ipConfigs {
		format := config.FrontendIPConfigurationPropertiesFormat
		if format != nil {
			brief := network.FrontendIPConfiguration{
				Name: config.Name,
				FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
					Subnet: format.Subnet,
				},
			}
			if format.PrivateIPAddress != nil {
				brief.PrivateIPAddress = format.PrivateIPAddress
				brief.PrivateIPAllocationMethod = format.PrivateIPAllocationMethod
			} else {
				brief.PublicIPAddress = format.PublicIPAddress
			}
			briefs = append(briefs, brief)
		}
	}
	return &briefs
}

// check probe backend and frontend in same config except chore for example Etag、ProvisioningState
func ensureSyncDefaultConfigExceptRules(azlb *network.LoadBalancer, lb *lbapi.LoadBalancer) (*network.LoadBalancer, bool, error) {
	var change bool
	// 1.ensure probe
	probes := defaultClusterAzureLBProbes()
	probesBrief := getProbesBrief(azlb.Probes)
	equal, err := isEqual(&probes, probesBrief)
	if err != nil {
		return azlb, change, err
	}
	if !equal {
		log.Infof("probes is change %v \n %v \n", probes, probesBrief)
		azlb.Probes = &probes
		change = true
	}

	// 2.check backend
	if azlb.BackendAddressPools == nil || len(*azlb.BackendAddressPools) == 0 {
		log.Infof("BackendAddressPools is change %v", *azlb.BackendAddressPools)
		azlb.BackendAddressPools = &[]network.BackendAddressPool{
			{
				Name: to.StringPtr(azureLoadBalancerBackendName),
			},
		}
		change = true
	}

	// 3.check frontend
	frontsConfig := getAzureLoadBalancerFrontendIPConfigByConfig(lb.Spec.Providers.Azure.IPAddressProperties)
	frontsBrief := getAzureLoadBalancerFrontendIPConfigBrief(azlb.FrontendIPConfigurations)
	equal, err = isEqual(frontsConfig, frontsBrief)
	if err != nil {
		return azlb, change, err
	}
	if !equal {
		log.Infof("frontsConfig is change %v \n %v \n", frontsConfig, frontsBrief)
		azlb.FrontendIPConfigurations = frontsConfig
		change = true
	}
	return azlb, change, nil
}

func isEqual(src, dest interface{}) (bool, error) {
	i, err := json.Marshal(src)
	if err != nil {
		return false, err
	}
	j, err := json.Marshal(dest)
	if err != nil {
		return false, err
	}
	if reflect.DeepEqual(i, j) {
		return true, nil
	}
	log.Infof("src %s \ndest %s \n", string(i), string(j))
	return false, nil
}

// create a default azure load balancer
func createAzureLoadBalancer(c *client.Client, lb *lbapi.LoadBalancer) (*network.LoadBalancer, error) {
	// init
	azlb := initClusterAzureLoadBalancerObject(lb, lb.Spec.Providers.Azure.ClusterID)
	log.Infof("create azure loadbalancer %s", to.String(azlb.Name))
	azlb, err := c.LoadBalancer.CreateOrUpdate(context.TODO(), lb.Spec.Providers.Azure.ResourceGroupName, to.String(azlb.Name), azlb)
	if err != nil {
		log.Errorf("create lb failed error : %v", err)
		return nil, err
	}
	log.Infof("create load balancer %s successfully", to.String(azlb.Name))
	return &azlb, nil
}

// ensure sync default rules
func ensureSyncDefaultRules(c *client.Client, azlb *network.LoadBalancer, lb *lbapi.LoadBalancer) (*network.LoadBalancer, error) {
	frontendIPConfigurationID := to.String((*azlb.FrontendIPConfigurations)[0].ID)
	backendAddressPoolID := to.String((*azlb.BackendAddressPools)[0].ID)
	lbRules := azureLBProbes2Rules(frontendIPConfigurationID, backendAddressPoolID, *azlb.Probes)
	var change bool
	if azlb.LoadBalancingRules != nil && len(*azlb.LoadBalancingRules) != 0 {
		change = patchAzureLoadBalancerDefaultRules(azlb, lbRules)
	} else {
		azlb.LoadBalancingRules = &lbRules
		change = true
	}
	if !change {
		return azlb, nil
	}
	var err error
	log.Infof("update default lb rules nums %d...", len(*azlb.LoadBalancingRules))
	*azlb, err = c.LoadBalancer.CreateOrUpdate(context.TODO(), lb.Spec.Providers.Azure.ResourceGroupName, to.String(azlb.Name), *azlb)
	if err != nil {
		log.Errorf(" update LoadBalancer failed, %v", err)
		return azlb, err
	}
	return azlb, nil
}

// patchAzureLoadBalancerDefaultRules
// patch default rules if the rule not exist in azure rules
func patchAzureLoadBalancerDefaultRules(azlb *network.LoadBalancer, defaultRules []network.LoadBalancingRule) bool {
	var change bool
	for _, defaultRule := range defaultRules {
		exist := false
		for _, rule := range *azlb.LoadBalancingRules {
			if equalRule(rule, defaultRule) {
				exist = true
				break
			}
		}
		if !exist {
			*azlb.LoadBalancingRules = append(*azlb.LoadBalancingRules, defaultRule)
			change = true
		}
	}
	return change
}

// check rule in same config except chore for example Etag、ProvisioningState
func equalRule(x, brief network.LoadBalancingRule) bool {
	Xbrief := network.LoadBalancingRule{
		Name: x.Name,
		LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{
			FrontendIPConfiguration: &network.SubResource{
				ID: x.FrontendIPConfiguration.ID,
			},
			BackendAddressPool: &network.SubResource{
				ID: x.BackendAddressPool.ID,
			},
			Probe: &network.SubResource{
				ID: x.Probe.ID,
			},
			Protocol:             x.Protocol,
			LoadDistribution:     x.LoadDistribution,
			FrontendPort:         x.FrontendPort,
			BackendPort:          x.BackendPort,
			IdleTimeoutInMinutes: x.IdleTimeoutInMinutes,
			EnableFloatingIP:     x.EnableFloatingIP,
			DisableOutboundSnat:  x.DisableOutboundSnat,
		},
	}
	return reflect.DeepEqual(Xbrief, brief)
}

func getAzureLoadbalancer(c *client.Client, groupName, lbName string) (*network.LoadBalancer, error) {
	if len(lbName) == 0 {
		return nil, nil
	}
	lb, err := c.LoadBalancer.Get(context.TODO(), groupName, lbName, "")
	if client.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		log.Errorf("get lb %s failed: %v", lbName, err)
		return nil, err
	}
	return &lb, nil
}

// ensureSyncRulesAndBackendPools return true if lb has diff with azureLB
// sync rules and BackendPools
func ensureSyncRulesAndBackendPools(c *client.Client, storeLister *core.StoreLister, azlb *network.LoadBalancer, lb *lbapi.LoadBalancer, tcpMap, udpMap map[string]string) error {

	groupName := getGroupName(lb)
	err := syncRules(c, azlb, tcpMap, udpMap, groupName)
	if err != nil {
		return err
	}
	log.Info("sync rules successfully...")
	detachNetworks, azlbBackendNetworksMap, err := syncBackendPools(c, storeLister, azlb, lb)
	if err != nil {
		return err
	}
	log.Infof("sync backendPools successfully...")
	// if use public address , need sync security rules to the security group
	if usePublicAddress(lb) {
		// add default security group rules
		tcpTemp := copyMap(tcpMap)
		udpTemp := copyMap(udpMap)
		// add default port
		tcpTemp["80"] = ""
		tcpTemp["443"] = ""
		err = syncSecurityGroupRules(c, tcpTemp, udpTemp, detachNetworks, azlbBackendNetworksMap)
		log.Infof("sync security group rules result %v", err)
	}
	return err
}

func syncSecurityGroupRules(c *client.Client, tcp, udp map[string]string, detachs []string, networks networkInterfaceIDSet) error {
	// get the sg to be delete and sync
	deleteSg, syncSg, err := getSuitableSecurityGroup(c, detachs, networks)
	if err != nil {
		return err
	}
	// delete the useless rules from security group
	err = deleteUselessSecurityRules(c, deleteSg)
	if err != nil {
		return err
	}

	return ensureSyncRulesToSecurityGroups(c, syncSg, tcp, udp)
}

// ensureSyncRulesToSecurityGroups sync security group rules
func ensureSyncRulesToSecurityGroups(c *client.Client, sgIDs securityGroupIDSet, tcp, udp map[string]string) error {
	for sgID := range sgIDs {
		log.Infof("update sg id %s", sgID)
		groupName, name, err := getGroupAndResourceNameFromID(sgID, azureSecurityGroups)
		if err != nil {
			return err
		}
		sg, err := c.SecurityGroup.Get(context.TODO(), groupName, name, "")
		if err != nil {
			return err
		}
		newRules := make([]network.SecurityRule, 0, len(*sg.SecurityRules)+len(tcp)+len(udp))
		var change bool

		// priority value is unique in the sg rules
		// store used and be deleted priority value
		sgUsedPriorityMap := make(map[int32]struct{})
		sgDeletePriorityMap := make(map[int32]struct{})

		if sg.SecurityRules != nil && len(*sg.SecurityRules) != 0 {
			for i := range *sg.SecurityRules {
				rule := (*sg.SecurityRules)[i]
				modify, remain := ensureSyncWithDefaultSetting(&rule, tcp, udp)
				if remain {
					newRules = append(newRules, rule)
					sgUsedPriorityMap[to.Int32(rule.Priority)] = struct{}{}
				} else {
					sgDeletePriorityMap[to.Int32(rule.Priority)] = struct{}{}
				}
				if modify {
					change = true
				}
			}
		}
		if len(udp) != 0 || len(tcp) != 0 {
			change = true
		}

		for port := range udp {
			priority, err := getValidPriority(sgUsedPriorityMap, sgDeletePriorityMap)
			if err != nil {
				return err
			}
			sgUsedPriorityMap[priority] = struct{}{}
			rule := getDefaultSecurityGroupRule(securityGroupUDPPrefix, port, to.Int32Ptr(priority))
			newRules = append(newRules, *rule)
		}

		for port := range tcp {
			priority, err := getValidPriority(sgUsedPriorityMap, sgDeletePriorityMap)
			if err != nil {
				return err
			}
			sgUsedPriorityMap[priority] = struct{}{}
			rule := getDefaultSecurityGroupRule(securityGroupTCPPrefix, port, to.Int32Ptr(priority))
			newRules = append(newRules, *rule)
		}
		if change {
			sg.SecurityRules = &newRules
			_, err = c.SecurityGroup.CreateOrUpdate(context.TODO(), groupName, name, sg)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// TODO need more priority? now max number of valid  priority value is 310
func getValidPriority(used, del map[int32]struct{}) (int32, error) {
	// use the be deleted priority value first
	for pri := range del {
		delete(del, pri)
		return pri, nil
	}
	// remain the high priority value to user for custom config
	priority := defaultSecurityGroupPriorityStart
	for {
		if priority > maxSecurityGroupPriority {
			return 0, fmt.Errorf("get valid priority failed, suggest to delete useless rule from cps")
		}
		_, ok := used[priority]
		if !ok {
			break
		}
		// remain interval to add new priority without modify current rules
		priority += defaultSecurityGroupPriorityIncrease
	}
	return priority, nil
}

// ensureSyncWithDefaultSetting reset the rules which automatically generate by cps if has differents
// between them and the default rules
func ensureSyncWithDefaultSetting(rule *network.SecurityRule, tcp, udp map[string]string) (bool, bool) {
	var ruleMap map[string]string
	var rulePrefix string
	// the port range is not the unique value during the security group rules
	// so we can't tell the rules which automatically generate by cps from all the sg rules by port range
	// now the criterion is using the specify prefix
	if strings.HasPrefix(to.String(rule.Name), securityGroupTCPPrefix) {
		ruleMap = tcp
		rulePrefix = securityGroupTCPPrefix
	} else if strings.HasPrefix(to.String(rule.Name), securityGroupUDPPrefix) {
		ruleMap = udp
		rulePrefix = securityGroupUDPPrefix
	} else {
		// remain others rules
		return false, true
	}

	_, ok := ruleMap[to.String(rule.DestinationPortRange)]
	// if the rule don't exist in the rule map return directly
	if !ok {
		return true, false
	}
	change := ensureSettingDefaultRules(rule, ruleMap, rulePrefix)
	// delete the exist port range, the rest of port will be added to sg rules
	delete(ruleMap, to.String(rule.DestinationPortRange))
	return change, true
}

// ensureSettingDefaultRules make sure the setting is default
func ensureSettingDefaultRules(rule *network.SecurityRule, tcp map[string]string, prefix string) bool {
	_ = tcp
	defaultRule := getDefaultSecurityGroupRule(prefix, to.String(rule.DestinationPortRange), rule.Priority)
	ruleBrief := getSecurityGroupRuleBrief(rule)
	if reflect.DeepEqual(defaultRule, ruleBrief) {
		return false
	}
	copySecurityGroup(rule, defaultRule)
	return true
}

func copySecurityGroup(dst, src *network.SecurityRule) {
	dst.Name = src.Name
	dst.Description = src.Description
	dst.Protocol = src.Protocol
	dst.SourcePortRange = src.SourcePortRange
	dst.DestinationPortRange = src.DestinationPortRange
	dst.SourceAddressPrefix = src.SourceAddressPrefix
	dst.DestinationAddressPrefix = src.DestinationAddressPrefix
	dst.Access = src.Access
	dst.Priority = src.Priority
	dst.Direction = src.Direction
}

func getSecurityGroupRuleBrief(rule *network.SecurityRule) *network.SecurityRule {
	return &network.SecurityRule{
		Name: rule.Name,
		SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
			Description:              rule.Description,
			Protocol:                 rule.Protocol,
			SourcePortRange:          rule.SourcePortRange,
			DestinationPortRange:     rule.DestinationPortRange,
			SourceAddressPrefix:      rule.SourceAddressPrefix,
			DestinationAddressPrefix: rule.DestinationAddressPrefix,
			Access:                   rule.Access,
			Priority:                 rule.Priority,
			Direction:                rule.Direction,
		},
	}
}

func getDefaultSecurityGroupRule(prefix, port string, priority *int32) *network.SecurityRule {
	protocl := network.SecurityRuleProtocolTCP
	if prefix == securityGroupUDPPrefix {
		protocl = network.SecurityRuleProtocolUDP
	}
	return &network.SecurityRule{
		Name: to.StringPtr(prefix + port),
		SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
			Description:              to.StringPtr("use for cps"),
			Protocol:                 protocl,
			SourcePortRange:          to.StringPtr("*"),
			DestinationPortRange:     to.StringPtr(port),
			SourceAddressPrefix:      to.StringPtr("*"),
			DestinationAddressPrefix: to.StringPtr("*"),
			Access:                   network.SecurityRuleAccessAllow,
			Priority:                 priority,
			Direction:                network.SecurityRuleDirectionInbound,
		},
	}
}

// deleteUselessSecurityRules delete the useless rules from security group
func deleteUselessSecurityRules(c *client.Client, sgIDs securityGroupIDSet) error {
	for sgID := range sgIDs {
		groupName, name, err := getGroupAndResourceNameFromID(sgID, azureSecurityGroups)
		if err != nil {
			return err
		}
		sg, err := c.SecurityGroup.Get(context.TODO(), groupName, name, "")
		if err != nil {
			return err
		}
		if sg.SecurityRules != nil && len(*sg.SecurityRules) != 0 {
			newRules := make([]network.SecurityRule, 0, len(*sg.SecurityRules))
			var change bool
			for i := range *sg.SecurityRules {
				rule := (*sg.SecurityRules)[i]
				// the port range is not the unique value during the security group rules
				// so we can't tell the rules which automatically generate by cps from all the sg rules by port range
				// now the criterion is using the specify prefix
				if strings.HasPrefix(to.String(rule.Name), securityGroupTCPPrefix) ||
					strings.HasPrefix(to.String(rule.Name), securityGroupUDPPrefix) {
					change = true
				} else {
					newRules = append(newRules, rule)
				}
			}
			if change {
				sg.SecurityRules = &newRules
				_, err = c.SecurityGroup.CreateOrUpdate(context.TODO(), groupName, name, sg)
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// getSuitableSecurityGroup get need to be deleted and sync security group
func getSuitableSecurityGroup(c *client.Client, detachs []string, networks networkInterfaceIDSet) (securityGroupIDSet, securityGroupIDSet, error) {
	deleteSg := make(securityGroupIDSet)
	syncSg := make(securityGroupIDSet)
	for _, network := range detachs {
		sg, err := getSecurityGroupIDFromNetWork(c, network)
		if err != nil {
			return nil, nil, err
		}
		deleteSg[sg] = struct{}{}
	}

	for network := range networks {
		sg, err := getSecurityGroupIDFromNetWork(c, network)
		if err != nil {
			return nil, nil, err
		}
		syncSg[sg] = struct{}{}
	}

	// the networkInterfaces which need to be deleted and sync may depend on the same security group
	// delete security group from deletesg which exist in syncsg
	for k := range syncSg {
		_, ok := deleteSg[k]
		if ok {
			delete(deleteSg, k)
		}
	}
	return deleteSg, syncSg, nil
}

func getSecurityGroupIDFromNetWork(c *client.Client, network string) (string, error) {
	groupName, name, err := getGroupAndResourceNameFromID(network, azureNetworkInterfaces)
	if err != nil {
		return "", err
	}
	net, err := c.NetworkInterface.Get(context.TODO(), groupName, name, "")
	if err != nil {
		return "", err
	}
	if net.NetworkSecurityGroup == nil {
		return "", fmt.Errorf("network %s has no network security group", network)
	}
	return to.String(net.NetworkSecurityGroup.ID), nil
}

// usePublicAddress tell lb if use public address
func usePublicAddress(lb *lbapi.LoadBalancer) bool {
	if lb.Spec.Providers.Azure == nil {
		return false
	}
	if lb.Spec.Providers.Azure.IPAddressProperties.Public != nil {
		return true
	}
	return false
}

func setProvisioningPublicIPAddress(lb *lbapi.LoadBalancer, publicIPAddress string) {
	if lb.Status.ProvidersStatuses.Azure == nil {
		lb.Status.ProvidersStatuses.Azure = &lbapi.AzureProviderStatus{}
	}
	lb.Status.ProvidersStatuses.Azure.PublicIPAddress = to.StringPtr(publicIPAddress)
}

func setProvisioningState(lb *lbapi.LoadBalancer, provisioningState string) {
	if lb.Status.ProvidersStatuses.Azure == nil {
		lb.Status.ProvidersStatuses.Azure = &lbapi.AzureProviderStatus{}
	}
	lb.Status.ProvidersStatuses.Azure.ProvisioningState = provisioningState
}

func syncRules(c *client.Client, azlb *network.LoadBalancer, tcpMap, udpMap map[string]string, groupName string) (err error) {
	log.Infof("sync rules azlb group %s name %s , \ntcp rules :%v \nudp rules: %v", groupName, to.String(azlb.Name), tcpMap, udpMap)
	tcpTemp := copyMap(tcpMap)
	udpTemp := copyMap(udpMap)
	change := makeUpRules(azlb, tcpTemp, udpTemp)
	if change {
		lb, err := c.LoadBalancer.CreateOrUpdate(context.TODO(), groupName, to.String(azlb.Name), *azlb)
		if err != nil {
			log.Errorf("update azure lb failed:%v", err)
			return err
		}
		azlb = &lb
	}
	return nil
}

// sync backend pools
func syncBackendPools(c *client.Client, storeLister *core.StoreLister, azlb *network.LoadBalancer, lb *lbapi.LoadBalancer) ([]string, networkInterfaceIDSet, error) {
	nodes := lb.Spec.Nodes.Names
	return syncBackendPoolsWithNodes(c, storeLister, azlb, nodes)
}

// sync backend pools with spec nodes
func syncBackendPoolsWithNodes(c *client.Client, storeLister *core.StoreLister, azlb *network.LoadBalancer, nodes []string) ([]string, networkInterfaceIDSet, error) {
	log.Infof("sync backend pools azlb name %s", to.String(azlb.Name))
	// nodes := lb.Spec.Nodes.Names
	detachs, attachs, azlbBackendNetworksMap, err := diffBackendPoolNetworkInterfaecs(c, azlb, nodes, storeLister)
	if err != nil {
		return nil, nil, err
	}
	if azlb.BackendAddressPools == nil || len(*azlb.BackendAddressPools) == 0 {
		return nil, nil, fmt.Errorf("backend pools is empty")
	}
	poolID := to.String((*(azlb.BackendAddressPools))[0].ID)
	for _, detach := range detachs {
		err := detachNetworkInterfacesAndLoadBalancer(c, detach, poolID)
		if err != nil {
			return nil, nil, err
		}
	}
	for _, attach := range attachs {
		err := attachNetworkInterfacesAndLoadBalancer(c, attach, poolID)
		if err != nil {
			return nil, nil, err
		}
	}
	return detachs, azlbBackendNetworksMap, nil
}

func detachNetworkInterfacesAndLoadBalancer(c *client.Client, detachID string, poolID string) error {
	log.Infof("detach network %s and backend pool %s", detachID, poolID)
	groupName, name, err := getGroupAndResourceNameFromID(detachID, azureNetworkInterfaces)
	if err != nil {
		return err
	}
	nic, err := c.NetworkInterface.Get(context.TODO(), groupName, name, "")
	if err != nil {
		return err
	}
	if nic.IPConfigurations == nil {
		log.Warnf("networkInterface[%s/%s] ipc is zero, skip it", name)
		return nil
	}

	ipcs := *(nic.IPConfigurations)
	needUpdate := false

	for i := range ipcs {
		if ipcs[i].LoadBalancerBackendAddressPools == nil || 0 == len(*(ipcs[i].LoadBalancerBackendAddressPools)) {
			continue
		}
		pools := *(ipcs[i].LoadBalancerBackendAddressPools)
		newPools := make([]network.BackendAddressPool, 0, len(pools))
		found := false
		for _, pool := range pools {
			if strings.ToLower(to.String(pool.ID)) == strings.ToLower(poolID) {
				found = true
			} else {
				newPools = append(newPools, pool)
			}
		}
		if found {
			ipcs[i].LoadBalancerBackendAddressPools = &newPools
			needUpdate = true
		}
	}
	if needUpdate {
		_, err := c.NetworkInterface.CreateOrUpdate(context.TODO(), groupName, name, nic)
		if err != nil {
			log.Errorf("update networkInterfaces error %v", err)
			return err
		}
	}
	return nil
}

func attachNetworkInterfacesAndLoadBalancer(c *client.Client, attachID string, poolID string) error {
	log.Infof("attach network %s and backend pool %s", attachID, poolID)
	groupName, name, err := getGroupAndResourceNameFromID(attachID, azureNetworkInterfaces)
	if err != nil {
		return err
	}
	nic, err := c.NetworkInterface.Get(context.TODO(), groupName, name, "")
	if err != nil {
		return err
	}
	if nic.IPConfigurations == nil {
		log.Warnf("networkInterface[%s/%s] ipc is zero, skip it", name)
		return fmt.Errorf("ipc is zero")
	}

	ipcs := *(nic.IPConfigurations)
	needUpdate := false

	for i := range ipcs {
		found := false
		pools := ipcs[i].LoadBalancerBackendAddressPools
		if pools != nil && len(*pools) != 0 {
			for _, pool := range *pools {
				if strings.ToLower(to.String(pool.ID)) == strings.ToLower(poolID) {
					found = true
					break
				}
			}
		}
		if !found {
			if pools == nil {
				newPools := make([]network.BackendAddressPool, 0, 1)
				pools = &newPools
			}
			*pools = append(*pools, network.BackendAddressPool{
				ID: to.StringPtr(poolID),
			})
			ipcs[i].LoadBalancerBackendAddressPools = pools
			needUpdate = true
		}
	}
	if needUpdate {
		_, err := c.NetworkInterface.CreateOrUpdate(context.TODO(), groupName, name, nic)
		if err != nil {
			log.Errorf("update networkInterfaces error %v", err)
			return err
		}
	}
	return nil
}

// init azure loadbalancer with no probe rules and backends
func initClusterAzureLoadBalancerObject(lb *lbapi.LoadBalancer, clusterID string) network.LoadBalancer {
	lbName := lb.Spec.Providers.Azure.Name
	if len(lbName) == 0 {
		lbName = getAzureLBName(lb.Name, clusterID)
	}
	azlb := network.LoadBalancer{
		Name:     to.StringPtr(lbName),
		Location: to.StringPtr(lb.Spec.Providers.Azure.Location),
		Sku: &network.LoadBalancerSku{
			Name: network.LoadBalancerSkuName(lb.Spec.Providers.Azure.SKU),
		},
		LoadBalancerPropertiesFormat: &network.LoadBalancerPropertiesFormat{},
	}
	frontendIPConfigurations := getAzureLoadBalancerFrontendIPConfigByConfig(lb.Spec.Providers.Azure.IPAddressProperties)
	azlb.FrontendIPConfigurations = frontendIPConfigurations
	return azlb
}

// cleanup
func recoverDefaultAzureLoadBalancer(c *client.Client, groupName, lbName string) (err error) {
	log.Infof("recover default azlb group %s name %s", groupName, lbName)
	defer func() {
		if err != nil {
			log.Errorf("recoverDefaultAzureLoadBalancer failed %v", err)
		}
	}()
	azlb, err := c.LoadBalancer.Get(context.TODO(), groupName, lbName, "")
	// if the lb is deleted by others, dont't return err
	if client.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	// 1.clean all rules
	if azlb.LoadBalancingRules != nil && len(*azlb.LoadBalancingRules) != 0 {
		*azlb.LoadBalancingRules = (*azlb.LoadBalancingRules)[:0]
	}

	// 2.clean probe
	if azlb.Probes != nil && len(*azlb.Probes) != 0 {
		*azlb.Probes = (*azlb.Probes)[:0]
	}

	pools := azlb.BackendAddressPools
	// update azlb
	log.Infof("start recover default azure loadbalancer lb")
	if pools != nil && len(*pools) != 0 {
		// 3.clean backend pools netInterfaces
		pool := (*pools)[0]
		poolID := to.String(pool.ID)

		// 4.detach all netInterfaces with backend pool ID
		if pool.BackendIPConfigurations != nil && len(*pool.BackendIPConfigurations) != 0 {
			log.Infof("detach BackendIPConfigurations len %v to poolID %s", len(*pool.BackendIPConfigurations), poolID)
			configs := *pool.BackendIPConfigurations
			for _, config := range configs {
				err = detachNetworkInterfacesAndLoadBalancer(c, to.String(config.ID), poolID)
				if err != nil {
					return err
				}
			}
		}
	}
	_, err = c.LoadBalancer.CreateOrUpdate(context.TODO(), groupName, lbName, azlb)
	if err != nil {
		return err
	}
	return nil
}
