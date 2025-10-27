/*
Copyright 2024 NVIDIA.

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

package dpucniprovisioner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/nvidia/doca-platform/pkg/utils/networkhelper"
	"github.com/nvidia/ovn-kubernetes-components/internal/constants"
	"github.com/nvidia/ovn-kubernetes-components/internal/utils/ovsclient"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
	kexec "k8s.io/utils/exec"
	"k8s.io/utils/ptr"
)

type Mode string

// String returns the string representation of the mode
func (m Mode) String() string {
	return string(m)
}

const (
	// InternalIPAM is the mode where IPAM is managed by DPUServiceIPAM objects and we have to provide the IPs to br-ovn
	// and the PF on the host
	InternalIPAM Mode = "internal-ipam"
	// ExternalIPAM is the mode where an external DHCP server provides the IPs to the br-ovn and PF on the host
	ExternalIPAM Mode = "external-ipam"
	// geneveHeaderSize is the size of the geneve header, which is 60 bytes.
	geneveHeaderSize = 60
	// maxMTUSize is the maximum MTU size that can be set on a network interface.
	maxMTUSize = 9216
	// flannelInterface is the flannel interface that has an IP address in the network where the pods on the local node
	// will get IPs from.
	flannelInterface = "cni0"
	// oobInterface is the out of band interface used by the DPU. This interface is hardcoded to the bridge name used in
	// the provisioning controller.
	// TODO: Consider discovering that in the future
	oobInterface = "br-comm-ch"
	// sourceRoutingTable is the route table used for source routing
	sourceRoutingTable = 60
)

const (
	// brOVN is the name of the bridge that is used by OVN as the external bridge (br-ex). This is the bridge that is
	// later connected with br-sfc. In the current OVN IC w/ DPU implementation, the internal port of this bridge acts
	// as the VTEP.
	brOVN = "br-ovn"
	brDPU = "br-dpu"

	// ovnkInputPath is the path to the file in which ovnkube-controller expects the additional gateway opts
	ovnkInputPath = "/etc/openvswitch/ovn_k8s.conf"
	// brOVNNetplanConfigPath is the path to the file which contains the netplan configuration for br-ovn
	brOVNNetplanConfigPath = "/etc/netplan/80-br-ovn.yaml"
	// netplanApplyDonePath is a file that indicates that a netplan apply has already ran and was successful. The content
	// of the file is used to create a cooldown period before re-issuing a subsequent netplan apply command.
	netplanApplyDonePath = "/etc/netplan/.dpucniprovisioner.done"
	// netplanApplyCooldownDuration determines the cooldown period of a successful netplan apply command before a
	// subsequent netplan apply command is executed.
	netplanApplyCooldownDuration = time.Minute * 2
)

type DPUCNIProvisioner struct {
	ctx                       context.Context
	clock                     clock.Clock
	ensureConfigurationTicker clock.Ticker
	ovsClient                 ovsclient.OVSClient
	networkHelper             networkhelper.NetworkHelper
	exec                      kexec.Interface
	kubernetesClient          kubernetes.Interface

	// FileSystemRoot controls the file system root. It's used for enabling easier testing of the package. Defaults to
	// empty.
	FileSystemRoot string

	// vtepIPNet is the IP that should be added to the VTEP interface.
	vtepIPNet *net.IPNet
	// gateway is the gateway IP that is configured on the routes related to OVN Kubernetes reaching its peer nodes
	// when traffic needs to go from one Pod running on Node A to another Pod running on Node B.
	gateway net.IP
	// vtepCIDR is the CIDR in which all the VTEP IPs of all the DPUs in the DPU cluster belong to. This CIDR is
	// configured on the routes related to traffic that needs to go from one Pod running on worker Node A to another Pod
	// running on worker Node B.
	vtepCIDR *net.IPNet
	// hostCIDR is the CIDR of the host machines that is configured on the routes related to OVN Kubernetes reaching
	// its peer nodes when traffic needs to go from one Pod running on worker Node A to another Pod running on control
	// plane A (and vice versa).
	hostCIDR *net.IPNet
	// pfIP is the IP that should be added to the PF on the host
	pfIP *net.IPNet
	// dpuHostName is the name of the DPU.
	dpuHostName string
	// gatewayDiscoveryNetwork is the network from which the DPUCNIProvisioner discovers the gateway that it should be
	// on relevant underlying systems.
	gatewayDiscoveryNetwork *net.IPNet

	// dhcpCmd is the struct that holds information about the DHCP Server process
	dhcpCmd kexec.Cmd
	// mode is the mode in which the CNI provisioner is running
	mode Mode
	// ovnMTU is the MTU that is configured for OVN
	ovnMTU int
}

// New creates a DPUCNIProvisioner that can configure the system
func New(ctx context.Context,
	mode Mode,
	clock clock.WithTicker,
	ovsClient ovsclient.OVSClient,
	networkHelper networkhelper.NetworkHelper,
	exec kexec.Interface,
	kubernetesClient kubernetes.Interface,
	vtepIPNet *net.IPNet,
	gateway net.IP,
	vtepCIDR *net.IPNet,
	hostCIDR *net.IPNet,
	pfIP *net.IPNet,
	dpuHostName string,
	gatewayDiscoveryNetwork *net.IPNet,
	ovnMTU int,
) *DPUCNIProvisioner {
	return &DPUCNIProvisioner{
		ctx:                       ctx,
		clock:                     clock,
		ensureConfigurationTicker: clock.NewTicker(30 * time.Second),
		ovsClient:                 ovsClient,
		networkHelper:             networkHelper,
		exec:                      exec,
		kubernetesClient:          kubernetesClient,
		FileSystemRoot:            "",
		vtepIPNet:                 vtepIPNet,
		gateway:                   gateway,
		vtepCIDR:                  vtepCIDR,
		hostCIDR:                  hostCIDR,
		pfIP:                      pfIP,
		dpuHostName:               dpuHostName,
		mode:                      mode,
		gatewayDiscoveryNetwork:   gatewayDiscoveryNetwork,
		ovnMTU:                    ovnMTU,
	}
}

// RunOnce runs the provisioning flow once and exits
func (p *DPUCNIProvisioner) RunOnce() error {
	if err := p.configure(); err != nil {
		return err
	}
	klog.Info("Configuration complete.")
	if p.mode == InternalIPAM {
		if err := p.startDHCPServer(); err != nil {
			return fmt.Errorf("error while starting DHCP server: %w", err)
		}
		klog.Info("DHCP Server started.")
	}

	return nil
}

// Stop stops the provisioner
func (p *DPUCNIProvisioner) Stop() {
	if p.mode == InternalIPAM {
		p.dhcpCmd.Stop()
	}

	klog.Info("Provisioner stopped")
}

// EnsureConfiguration ensures that particular configuration is in place. This is a blocking function.
func (p *DPUCNIProvisioner) EnsureConfiguration() {
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-p.ensureConfigurationTicker.C():
			if err := p.configure(); err != nil {
				klog.Errorf("failed to ensure configuration: %s", err.Error())
			}
		}
	}
}

// configure runs the provisioning flow once
func (p *DPUCNIProvisioner) configure() error {
	klog.Info("Configuring Kubernetes host name in OVS")
	if err := p.findAndSetKubernetesHostNameInOVS(); err != nil {
		return fmt.Errorf("error while setting the Kubernetes Host Name in OVS: %w", err)
	}

	if p.mode == ExternalIPAM {
		klog.Info("Configuring br-ovn")
		if err := p.configureBROVN(); err != nil {
			return fmt.Errorf("error while configuring br-ovn: %w", err)
		}
	}

	klog.Info("Configuring system to enable pod to pod on different node connectivity")
	if err := p.configurePodToPodOnDifferentNodeConnectivity(); err != nil {
		return err
	}

	klog.Info("Writing OVN Kubernetes expected input files")
	if err := p.writeFilesForOVN(); err != nil {
		return err
	}

	klog.Info("Configuring symmetric routing")
	if err := p.configureSymmetricRouting(); err != nil {
		return err
	}

	return nil
}

// findAndSetKubernetesHostNameInOVS discovers and sets the Kubernetes Host Name in OVS
func (p *DPUCNIProvisioner) findAndSetKubernetesHostNameInOVS() error {
	nodeClient := p.kubernetesClient.CoreV1().Nodes()
	n, err := nodeClient.Get(p.ctx, p.dpuHostName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error while getting Kubernetes Node: %w", err)
	}
	hostName, ok := n.Labels[constants.HostNameDPULabelKey]
	if !ok {
		return fmt.Errorf("required label %s is not set on node %s in the DPU cluster", constants.HostNameDPULabelKey, p.dpuHostName)
	}

	if err := p.ovsClient.SetKubernetesHostNodeName(hostName); err != nil {
		return fmt.Errorf("error while setting the Kubernetes Host Name in OVS: %w", err)
	}
	return nil
}

// configurePodToPodOnDifferentNodeConnectivity configures the VTEP interface (br-ovn) and the ovn-encap-ip external ID
// so that traffic going through the geneve tunnels can function as expected.
func (p *DPUCNIProvisioner) configurePodToPodOnDifferentNodeConnectivity() error {
	if p.mode == InternalIPAM {
		if err := p.setLinkIPAddressIfNotSet(brOVN, p.vtepIPNet); err != nil {
			return fmt.Errorf("error while setting VTEP IP: %w", err)
		}
		if err := p.networkHelper.SetLinkUp(brOVN); err != nil {
			return fmt.Errorf("error while setting link %s up: %w", brOVN, err)
		}

		_, vtepNetwork, err := net.ParseCIDR(p.vtepIPNet.String())
		if err != nil {
			return fmt.Errorf("error while parsing network from VTEP IP %s: %w", p.vtepIPNet.String(), err)
		}

		if vtepNetwork.String() != p.vtepCIDR.String() {
			// Add route related to traffic that needs to go from one Pod running on worker Node A to another Pod running
			// on worker Node B.
			if err := p.addRouteIfNotExists(p.vtepCIDR, p.gateway, brOVN, nil, nil); err != nil {
				return fmt.Errorf("error while adding route %s %s %s: %w", p.vtepCIDR, p.gateway.String(), brOVN, err)
			}
		}
	}

	// Add route related to traffic that needs to go from one Pod running on worker Node A to another Pod running on
	// control plane A (and vice versa).
	//
	// In our setup, we will already have a route pointing to the same CIDR via the SF designated for kubelet traffic
	// which gets a DHCP IP in that CIDR. Given that, we need to set the metric of this route to something very high
	// so that it's the last preferred route in the route table for that CIDR. The reason for that is this OVS bug that
	// selects the route with the highest prio - see issue 3871067.
	if err := p.addRouteIfNotExists(p.hostCIDR, p.gateway, brOVN, ptr.To[int](10000), nil); err != nil {
		return fmt.Errorf("error while adding route %s %s %s: %w", p.hostCIDR, p.gateway.String(), brOVN, err)
	}

	if err := p.ovsClient.SetOVNEncapIP(p.vtepIPNet.IP); err != nil {
		return fmt.Errorf("error while setting the OVN Encap IP: %w", err)
	}

	return nil
}

// setLinkIPAddressIfNotSet sets an IP address to a link if it's not already set
func (p *DPUCNIProvisioner) setLinkIPAddressIfNotSet(link string, ipNet *net.IPNet) error {
	hasIP, err := p.networkHelper.LinkIPAddressExists(link, ipNet)
	if err != nil {
		return fmt.Errorf("error checking whether IP exists: %w", err)
	}
	if hasIP {
		klog.Infof("Link %s has IP %s, skipping configuration", link, ipNet)
		return nil
	}
	if err := p.networkHelper.SetLinkIPAddress(link, ipNet); err != nil {
		return fmt.Errorf("error setting IP address: %w", err)
	}
	return nil
}

// addRouteIfNotExists adds a route if it doesn't already exist
func (p *DPUCNIProvisioner) addRouteIfNotExists(network *net.IPNet, gateway net.IP, device string, metric *int, table *int) error {
	hasRoute, err := p.networkHelper.RouteExists(network, gateway, device, table)
	if err != nil {
		return fmt.Errorf("error checking whether route exists: %w", err)
	}
	if hasRoute {
		klog.Infof("Route %s %s %s with metric %v in table %v exists, skipping configuration", network, gateway, device, metric, table)
		return nil
	}
	if err := p.networkHelper.AddRoute(network, gateway, device, metric, table); err != nil {
		return fmt.Errorf("error adding route: %w", err)
	}
	return nil
}

// addRuleIfNotExists adds a rule if it doesn't already exist
func (p *DPUCNIProvisioner) addRuleIfNotExists(network *net.IPNet, table int, priority int) error {
	hasRule, err := p.networkHelper.RuleExists(network, table, priority)
	if err != nil {
		return fmt.Errorf("error checking whether rule exists: %w", err)
	}
	if hasRule {
		klog.Infof("Rule %v %d %d exists, skipping configuration", network, table, priority)
		return nil
	}
	if err := p.networkHelper.AddRule(network, table, priority); err != nil {
		return fmt.Errorf("error adding rule: %w", err)
	}
	return nil
}

// writeFilesForOVN writes the input files that the ovnkube-controller expects
func (p *DPUCNIProvisioner) writeFilesForOVN() error {
	configPath := filepath.Join(p.FileSystemRoot, ovnkInputPath)

	// Build the complete content in one operation
	content := "[Gateway]\n"
	content += p.writeOVNInputGatewayOptsFile()

	routerSubnetContent, err := p.writeOVNInputRouterSubnetPath()
	if err != nil {
		return fmt.Errorf("error while getting the gateway router subnet content: %w", err)
	}
	content += routerSubnetContent

	// Write the complete content to the file in one operation
	err = os.WriteFile(configPath, []byte(content), 0644)
	if err != nil {
		return fmt.Errorf("error writing to file %s: %w", configPath, err)
	}

	return nil
}

// configureBROVN requests an IP via DHCP for br-ovn and mutates the relevant fields of the DPUCNIProvisioner objects
func (p *DPUCNIProvisioner) configureBROVN() error {
	if err := p.writeNetplanFileForBROVN(); err != nil {
		return fmt.Errorf("error while br-ovn netplan: %w", err)
	}

	addrs, err := p.networkHelper.GetLinkIPAddresses(brOVN)
	if err != nil {
		return fmt.Errorf("error while getting IP addresses for link %s: %w", brOVN, err)
	}

	if len(addrs) != 1 {
		if err := p.runNetplanApply(); err != nil {
			return fmt.Errorf("error running netplan apply: %w", err)
		}

		return fmt.Errorf("exactly 1 IP is expected in %s, but found %d", brOVN, len(addrs))
	}

	p.vtepIPNet = addrs[0]

	gateway, err := p.networkHelper.GetGateway(p.gatewayDiscoveryNetwork)
	if err != nil {
		return fmt.Errorf("error while parsing gateway from gateway discovery network %s: %w", p.gatewayDiscoveryNetwork.String(), err)
	}

	p.gateway = gateway
	return nil
}

// writeOVNInputGatewayOptsFile returns the gateway options content
// that ovnkube-controller reads from.
func (p *DPUCNIProvisioner) writeOVNInputGatewayOptsFile() string {
	return "next-hop=" + p.gateway.String() + "\n"
}

// writeOVNInputRouterSubnetPath returns the Gateway Router Subnet content
// that kubeovn-controller reads.
func (p *DPUCNIProvisioner) writeOVNInputRouterSubnetPath() (string, error) {
	_, vtepNetwork, err := net.ParseCIDR(p.vtepIPNet.String())
	if err != nil {
		return "", fmt.Errorf("error while parsing network from VTEP IP %s: %w", p.vtepIPNet.String(), err)
	}
	return "router-subnet=" + vtepNetwork.String() + "\n", nil
}

// writeNetplanFileForBROVN writes a netplan file for br-ovn to request dhcp
func (p *DPUCNIProvisioner) writeNetplanFileForBROVN() error {
	configPath := filepath.Join(p.FileSystemRoot, brOVNNetplanConfigPath)
	content := fmt.Sprintf(`
network:
  renderer: networkd
  version: 2
  bridges:
    %s:
      dhcp4: yes
      dhcp4-overrides:
        use-dns: no
      openvswitch: {}
`, brOVN)
	if err := os.WriteFile(configPath, []byte(content), 0600); err != nil {
		return fmt.Errorf("error while writing file %s: %w", configPath, err)
	}

	return nil
}

// runNetplanApply runs netplan apply while respecting the cooldown period after a successful netplan apply
func (p *DPUCNIProvisioner) runNetplanApply() error {
	applyDonePath := filepath.Join(p.FileSystemRoot, netplanApplyDonePath)
	lastSuccessfulRunTimestampRaw, err := os.ReadFile(applyDonePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("error while reading %s: %w", applyDonePath, err)
	}

	var lastSuccessfulRunTimestampRawInt int
	if string(lastSuccessfulRunTimestampRaw) != "" {
		lastSuccessfulRunTimestampRawInt, err = strconv.Atoi(string(lastSuccessfulRunTimestampRaw))
		if err != nil {
			return fmt.Errorf("error parsing timestamp from string %s: %w", lastSuccessfulRunTimestampRaw, err)
		}
	}

	lastSuccessfulRunTimestamp := time.Unix(int64(lastSuccessfulRunTimestampRawInt), 0)
	if lastSuccessfulRunTimestamp.Add(netplanApplyCooldownDuration).After(p.clock.Now()) {
		klog.Info("netplan apply is in cool down period, skipping apply")
		return nil
	}

	cmd := p.exec.Command("netplan", "apply")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.SetStdout(&stdout)
	cmd.SetStderr(&stderr)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("error running netplan: stdout='%s' stderr='%s': %w", stdout.String(), stderr.String(), err)
	}

	if err = os.WriteFile(applyDonePath, []byte(strconv.Itoa(int(p.clock.Now().Unix()))), 0644); err != nil {
		return fmt.Errorf("error writing file %s: %w", applyDonePath, err)
	}

	return nil
}

// startDHCPServer starts a DHCP Server to enable the PF on the host to get an IP.
func (p *DPUCNIProvisioner) startDHCPServer() error {
	if p.dhcpCmd != nil {
		klog.Warning("DHCP Server already running")
		return nil
	}

	_, vtepNetwork, err := net.ParseCIDR(p.vtepIPNet.String())
	if err != nil {
		return fmt.Errorf("error while parsing network from VTEP IP %s: %w", p.vtepIPNet.String(), err)
	}

	mac, err := p.networkHelper.GetPFRepMACAddress("pf0hpf")
	if err != nil {
		return fmt.Errorf("error while parsing MAC address of the PF on the host: %w", err)
	}

	// Add the geneve header size to the MTU.
	pfMTU := p.ovnMTU + geneveHeaderSize

	if pfMTU == geneveHeaderSize || pfMTU > maxMTUSize {
		return errors.New("invalid PF MTU: it must be greater than 60 and less than or equal to 9216")
	}

	args := []string{
		"--keep-in-foreground",
		"--port=0",         // Disable DNS Server
		"--log-facility=-", // Log to stderr
		fmt.Sprintf("--interface=%s", brOVN),
		"--dhcp-option=option:router",
		fmt.Sprintf("--dhcp-option=option:mtu,%d", pfMTU),
		fmt.Sprintf("--dhcp-range=%s,static", vtepNetwork.IP.String()),
		fmt.Sprintf("--dhcp-host=%s,%s", mac, p.pfIP.IP.String()),
	}

	if vtepNetwork.String() != p.vtepCIDR.String() {
		args = append(args, fmt.Sprintf("--dhcp-option=option:classless-static-route,%s,%s", p.vtepCIDR.String(), p.gateway.String()))
	}

	cmd := p.exec.Command("dnsmasq", args...)

	cmd.SetStdout(os.Stdout)
	cmd.SetStderr(os.Stderr)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("error while starting the DHCP server: %w", err)
	}

	p.dhcpCmd = cmd
	return nil
}

// configureSymmetricRouting configures source routing to avoid asymmetric routing for the following 2 flows:
// * When source address is an IP belonging to the DPU (OOB), traffic should always go back via the OOB
// * When source address is a primary CNI address of a Pod on the DPUCluster, traffic should always go back via the OOB
// This feature is essential so that DPUService ConfigPorts feature is working as expected.
func (p *DPUCNIProvisioner) configureSymmetricRouting() error {
	// When source address is a Pod on the DPUCluster, traffic should always go back via the OOB
	flannelInterfaceIPs, err := p.networkHelper.GetLinkIPAddresses(flannelInterface)
	if err != nil {
		return fmt.Errorf("error while getting IP addresses for link %s: %w", flannelInterface, err)
	}

	if len(flannelInterfaceIPs) != 1 {
		return fmt.Errorf("flannel interface %s is expected to have a single address", flannelInterface)
	}

	_, flannelNetwork, err := net.ParseCIDR(flannelInterfaceIPs[0].String())
	if err != nil {
		return fmt.Errorf("error while parsing the local CNI network: %w", err)
	}

	// ip rule a prio 31000 from 10.244.6.0/24 lookup 60
	if err := p.addRuleIfNotExists(flannelNetwork, sourceRoutingTable, 31000); err != nil {
		return fmt.Errorf("error while adding rule: %w", err)
	}

	// When source address is an IP belonging to the DPU (OOB), traffic should always go back via the OOB
	oobInterfaceIPs, err := p.networkHelper.GetLinkIPAddresses(oobInterface)
	if err != nil {
		return fmt.Errorf("error while getting IP addresses for link %s: %w", oobInterface, err)
	}

	if len(oobInterfaceIPs) != 1 {
		return fmt.Errorf("oob interface %s is expected to have a single address", oobInterface)
	}

	oobInterfaceIP := &net.IPNet{
		IP:   oobInterfaceIPs[0].IP,
		Mask: net.CIDRMask(32, 32),
	}

	// ip rule a prio 32000 from 10.0.110.70/32 lookup 60
	if err := p.addRuleIfNotExists(oobInterfaceIP, sourceRoutingTable, 32000); err != nil {
		return fmt.Errorf("error while adding rule: %w", err)
	}

	_, defaultRouteNetwork, err := net.ParseCIDR("0.0.0.0/0")
	if err != nil {
		return fmt.Errorf("error while parsing 0.0.0.0/0 as network: %w", err)
	}

	defaultGateway, err := p.networkHelper.GetGateway(defaultRouteNetwork)
	if err != nil {
		return fmt.Errorf("error while parsing gateway %s: %w", defaultRouteNetwork.String(), err)
	}

	// Add route referenced by the above rules
	// ip route a table 60 10.0.120.0/22 via 10.0.110.254 dev br-comm-ch
	if err := p.addRouteIfNotExists(p.vtepCIDR, defaultGateway, oobInterface, nil, ptr.To(sourceRoutingTable)); err != nil {
		return fmt.Errorf("error while adding rule: %w", err)
	}

	return nil
}
