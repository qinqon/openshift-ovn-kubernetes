package infraprovider

import (
	"encoding/xml"
	"fmt"
	"os"
	"strings"
	"text/template"
)

// AWS VPN Configuration XML types.
// These match the structure of the CustomerGatewayConfiguration XML
// returned by `aws ec2 describe-vpn-connections`.

type vpnConnection struct {
	XMLName    xml.Name    `xml:"vpn_connection"`
	ID         string      `xml:"id,attr"`
	Tunnels    []vpnTunnel `xml:"ipsec_tunnel"`
}

type vpnTunnel struct {
	CustomerGateway vpnTunnelGateway `xml:"customer_gateway"`
	VPNGateway      vpnTunnelGateway `xml:"vpn_gateway"`
	IKE             vpnTunnelIKE     `xml:"ike"`
	IPSec           vpnTunnelIPSec   `xml:"ipsec"`
}

type vpnTunnelGateway struct {
	TunnelOutsideAddress vpnTunnelAddress `xml:"tunnel_outside_address"`
	TunnelInsideAddress  vpnTunnelAddress `xml:"tunnel_inside_address"`
	BGP                  vpnTunnelBGP     `xml:"bgp"`
}

type vpnTunnelAddress struct {
	IPAddress string `xml:"ip_address"`
	NetworkMask string `xml:"network_mask"`
	NetworkCIDR string `xml:"network_cidr"`
}

type vpnTunnelBGP struct {
	ASN    string `xml:"asn"`
	HoldTime string `xml:"hold_time"`
}

type vpnTunnelIKE struct {
	AuthenticationProtocol string `xml:"authentication_protocol"`
	EncryptionProtocol     string `xml:"encryption_protocol"`
	Lifetime               string `xml:"lifetime"`
	PerfectForwardSecrecy  string `xml:"perfect_forward_secrecy"`
	Mode                   string `xml:"mode"`
	PreSharedKey           string `xml:"pre_shared_key"`
}

type vpnTunnelIPSec struct {
	Protocol               string `xml:"protocol"`
	AuthenticationProtocol string `xml:"authentication_protocol"`
	EncryptionProtocol     string `xml:"encryption_protocol"`
	Lifetime               string `xml:"lifetime"`
	PerfectForwardSecrecy  string `xml:"perfect_forward_secrecy"`
	Mode                   string `xml:"mode"`
	ClearDFBit             string `xml:"clear_df_bit"`
	FragmentationBeforeEncryption string `xml:"fragmentation_before_encryption"`
	TCPMSSAdjustment       string `xml:"tcp_mss_adjustment"`
	DeadPeerDetection      vpnDPD `xml:"dead_peer_detection"`
}

type vpnDPD struct {
	Interval string `xml:"interval"`
	Retries  string `xml:"retries"`
}

// AWSVPNConfig holds the parsed VPN configuration needed to set up
// strongSwan and FRR inside the external FRR container.
type AWSVPNConfig struct {
	Tunnels  []AWSVPNTunnelConfig
	LocalASN int
}

// AWSVPNTunnelConfig holds the configuration for a single IPsec tunnel.
type AWSVPNTunnelConfig struct {
	// AWS-side VPN endpoint public IP
	AWSEndpointIP string
	// Pre-shared key for IKE authentication
	PreSharedKey string
	// Inside tunnel APIPA addresses
	CGWInsideIP   string // Customer Gateway side (our side)
	AWSInsideIP   string // AWS side
	InsideCIDR    string // e.g., "/30"
	// BGP ASN of the AWS side
	AWSAsn string
}

// ParseVPNConfigFile reads and parses an AWS VPN configuration XML file.
func ParseVPNConfigFile(path string) (*AWSVPNConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read VPN config file %s: %w", path, err)
	}
	return ParseVPNConfigXML(data)
}

// ParseVPNConfigXML parses an AWS VPN configuration XML blob.
func ParseVPNConfigXML(data []byte) (*AWSVPNConfig, error) {
	var conn vpnConnection
	if err := xml.Unmarshal(data, &conn); err != nil {
		return nil, fmt.Errorf("failed to parse VPN config XML: %w", err)
	}

	if len(conn.Tunnels) == 0 {
		return nil, fmt.Errorf("no tunnels found in VPN config XML")
	}

	config := &AWSVPNConfig{
		LocalASN: 65000, // default, can be overridden
	}

	for i, tunnel := range conn.Tunnels {
		tc := AWSVPNTunnelConfig{
			AWSEndpointIP: tunnel.VPNGateway.TunnelOutsideAddress.IPAddress,
			PreSharedKey:  tunnel.IKE.PreSharedKey,
			CGWInsideIP:   tunnel.CustomerGateway.TunnelInsideAddress.IPAddress,
			AWSInsideIP:   tunnel.VPNGateway.TunnelInsideAddress.IPAddress,
			InsideCIDR:    tunnel.CustomerGateway.TunnelInsideAddress.NetworkCIDR,
			AWSAsn:        tunnel.VPNGateway.BGP.ASN,
		}

		if tc.AWSEndpointIP == "" {
			return nil, fmt.Errorf("tunnel %d: missing AWS endpoint IP", i)
		}
		if tc.PreSharedKey == "" {
			return nil, fmt.Errorf("tunnel %d: missing pre-shared key", i)
		}
		if tc.CGWInsideIP == "" || tc.AWSInsideIP == "" {
			return nil, fmt.Errorf("tunnel %d: missing inside tunnel addresses", i)
		}
		if tc.InsideCIDR == "" {
			tc.InsideCIDR = "/30" // default for AWS VPN tunnels
		}

		config.Tunnels = append(config.Tunnels, tc)
	}

	return config, nil
}

// GenerateSwanctlConf generates a swanctl.conf string from the parsed
// VPN configuration.
func (c *AWSVPNConfig) GenerateSwanctlConf() (string, error) {
	// Re-parse the template with the inc function available
	tmpl, err := template.New("swanctl").Funcs(template.FuncMap{
		"inc": func(i int) int { return i + 1 },
	}).Parse(`
connections {
{{- range $i, $t := .Tunnels }}
    aws-tunnel-{{ $i }} {
        version = 2
        local_addrs = 0.0.0.0
        remote_addrs = {{ $t.AWSEndpointIP }}
        dpd_delay = 10s
        dpd_timeout = 30s
        encap = yes
        rekey_time = 28800s

        local {
            auth = psk
        }
        remote {
            auth = psk
            id = {{ $t.AWSEndpointIP }}
        }

        children {
            aws-tunnel-{{ $i }} {
                local_ts = 0.0.0.0/0
                remote_ts = 0.0.0.0/0
                mode = tunnel
                start_action = start
                dpd_action = restart
                rekey_time = 3600s
                esp_proposals = aes128-sha1-modp1024,aes256-sha256-modp2048
                if_id_in = {{ inc $i }}
                if_id_out = {{ inc $i }}
            }
        }

        proposals = aes128-sha1-modp1024,aes256-sha256-modp2048
    }
{{- end }}
}

secrets {
{{- range $i, $t := .Tunnels }}
    ike-aws-{{ $i }} {
        id = {{ $t.AWSEndpointIP }}
        secret = "{{ $t.PreSharedKey }}"
    }
{{- end }}
}
`)
	if err != nil {
		return "", fmt.Errorf("failed to parse swanctl template: %w", err)
	}

	var buf strings.Builder
	if err := tmpl.Execute(&buf, c); err != nil {
		return "", fmt.Errorf("failed to generate swanctl.conf: %w", err)
	}
	return buf.String(), nil
}

// GenerateFRRBGPCommands generates vtysh commands to add TGW BGP peers
// to the running FRR instance.
func (c *AWSVPNConfig) GenerateFRRBGPCommands() []string {
	cmds := []string{
		"configure terminal",
		fmt.Sprintf("router bgp %d", c.LocalASN),
		"no bgp ebgp-requires-policy",
	}

	for _, t := range c.Tunnels {
		remoteASN := t.AWSAsn
		if remoteASN == "" {
			remoteASN = "64512"
		}
		cmds = append(cmds,
			fmt.Sprintf("neighbor %s remote-as %s", t.AWSInsideIP, remoteASN),
			fmt.Sprintf("neighbor %s timers 10 30", t.AWSInsideIP),
		)
	}

	cmds = append(cmds, "address-family ipv4 unicast")
	for _, t := range c.Tunnels {
		cmds = append(cmds,
			fmt.Sprintf("neighbor %s soft-reconfiguration inbound", t.AWSInsideIP),
		)
	}
	cmds = append(cmds, "exit-address-family", "end")

	return cmds
}

// XfrmInterfaceCommands generates the shell commands to create xfrm
// tunnel interfaces inside the container for each IPsec tunnel.
// XfrmInterfaceNames returns the xfrm interface names (e.g. vti1, vti2).
func (c *AWSVPNConfig) XfrmInterfaceNames() []string {
	var names []string
	for i := range c.Tunnels {
		names = append(names, fmt.Sprintf("vti%d", i+1))
	}
	return names
}

func (c *AWSVPNConfig) XfrmInterfaceCommands() [][]string {
	var cmds [][]string
	for i, t := range c.Tunnels {
		ifID := fmt.Sprintf("%d", i+1)
		ifName := fmt.Sprintf("vti%d", i+1)
		cidr := t.InsideCIDR
		if !strings.HasPrefix(cidr, "/") {
			cidr = "/" + cidr
		}

		cmds = append(cmds,
			[]string{"ip", "link", "add", ifName, "type", "xfrm", "if_id", ifID},
			[]string{"ip", "addr", "add", t.CGWInsideIP + cidr, "dev", ifName},
			[]string{"ip", "link", "set", ifName, "up"},
			[]string{"ip", "link", "set", ifName, "mtu", "1400"},
		)
	}
	return cmds
}
