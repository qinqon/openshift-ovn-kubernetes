//go:build linux
// +build linux

package metrics

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"k8s.io/klog/v2"
)

var (
	ovsVersion string
)

// ovs datapath Metrics
var metricOvsDpTotal = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: MetricOvsNamespace,
	Subsystem: MetricOvsSubsystemVswitchd,
	Name:      "dp_total",
	Help:      "Represents total number of datapaths on the system.",
})

var metricOvsDp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: MetricOvsNamespace,
	Subsystem: MetricOvsSubsystemVswitchd,
	Name:      "dp",
	Help: "A metric with a constant '1' value labeled by datapath " +
		"name present on the instance."},
	[]string{
		"datapath",
		"type",
	},
)

var metricOvsDpIfTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: MetricOvsNamespace,
	Subsystem: MetricOvsSubsystemVswitchd,
	Name:      "dp_if_total",
	Help:      "Represents the number of ports connected to the datapath."},
	[]string{
		"datapath",
	},
)

var metricOvsDpFlowsTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: MetricOvsNamespace,
	Subsystem: MetricOvsSubsystemVswitchd,
	Name:      "dp_flows_total",
	Help:      "Represents the number of flows in datapath."},
	[]string{
		"datapath",
	},
)

var metricOvsDpFlowsLookupHit = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: MetricOvsNamespace,
	Subsystem: MetricOvsSubsystemVswitchd,
	Name:      "dp_flows_lookup_hit",
	Help: "Represents number of packets matching the existing flows " +
		"while processing incoming packets in the datapath."},
	[]string{
		"datapath",
	},
)

var metricOvsDpFlowsLookupMissed = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: MetricOvsNamespace,
	Subsystem: MetricOvsSubsystemVswitchd,
	Name:      "dp_flows_lookup_missed",
	Help: "Represents the number of packets not matching any existing " +
		"flow  and require  user space processing."},
	[]string{
		"datapath",
	},
)

var metricOvsDpFlowsLookupLost = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: MetricOvsNamespace,
	Subsystem: MetricOvsSubsystemVswitchd,
	Name:      "dp_flows_lookup_lost",
	Help: "number of packets destined for user space process but " +
		"subsequently dropped before  reaching  userspace."},
	[]string{
		"datapath",
	},
)

var metricOvsDpPacketsTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: MetricOvsNamespace,
	Subsystem: MetricOvsSubsystemVswitchd,
	Name:      "dp_packets_total",
	Help: "Represents the total number of packets datapath processed " +
		"which is the sum of hit and missed."},
	[]string{
		"datapath",
	},
)

var metricOvsdpMasksHit = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: MetricOvsNamespace,
	Subsystem: MetricOvsSubsystemVswitchd,
	Name:      "dp_masks_hit",
	Help:      "Represents the total number of masks visited for matching incoming packets.",
},
	[]string{
		"datapath",
	},
)

var metricOvsDpMasksTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: MetricOvsNamespace,
	Subsystem: MetricOvsSubsystemVswitchd,
	Name:      "dp_masks_total",
	Help:      "Represents the number of masks in a datapath."},
	[]string{
		"datapath",
	},
)

var metricOvsDpMasksHitRatio = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: MetricOvsNamespace,
	Subsystem: MetricOvsSubsystemVswitchd,
	Name:      "dp_masks_hit_ratio",
	Help: "Represents the average number of masks visited per packet " +
		"the  ratio between hit and total number of packets processed by the datapath."},
	[]string{
		"datapath",
	},
)

// ovs bridge statistics & attributes metrics
var metricOvsBridgeTotal = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: MetricOvsNamespace,
	Subsystem: MetricOvsSubsystemVswitchd,
	Name:      "bridge_total",
	Help:      "Represents total number of OVS bridges on the system.",
},
)

var metricOvsBridge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: MetricOvsNamespace,
	Subsystem: MetricOvsSubsystemVswitchd,
	Name:      "bridge",
	Help: "A metric with a constant '1' value labeled by bridge name " +
		"present on the instance."},
	[]string{
		"bridge",
	},
)

var metricOvsBridgePortsTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: MetricOvsNamespace,
	Subsystem: MetricOvsSubsystemVswitchd,
	Name:      "bridge_ports_total",
	Help:      "Represents the number of OVS ports on the bridge."},
	[]string{
		"bridge",
	},
)

var metricOvsBridgeFlowsTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: MetricOvsNamespace,
	Subsystem: MetricOvsSubsystemVswitchd,
	Name:      "bridge_flows_total",
	Help:      "Represents the number of OpenFlow flows on the OVS bridge."},
	[]string{
		"bridge",
	},
)

// ovs interface metrics
var metricOvsInterfaceResetsTotal = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: MetricOvsNamespace,
	Subsystem: MetricOvsSubsystemVswitchd,
	Name:      "interface_resets_total",
	Help:      "The number of link state changes observed by Open vSwitch interface(s).",
})

var metricOvsInterfaceRxDroppedTotal = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: MetricOvsNamespace,
	Subsystem: MetricOvsSubsystemVswitchd,
	Name:      "interface_rx_dropped_total",
	Help:      "The total number of received packets dropped by Open vSwitch interface(s).",
})

var metricOvsInterfaceTxDroppedTotal = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: MetricOvsNamespace,
	Subsystem: MetricOvsSubsystemVswitchd,
	Name:      "interface_tx_dropped_total",
	Help:      "The total number of transmitted packets dropped by Open vSwitch interface(s).",
})

var metricOvsInterfaceRxErrorsTotal = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: MetricOvsNamespace,
	Subsystem: MetricOvsSubsystemVswitchd,
	Name:      "interface_rx_errors_total",
	Help:      "The total number of received packets with errors by Open vSwitch interface(s).",
})

var metricOvsInterfaceTxErrorsTotal = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: MetricOvsNamespace,
	Subsystem: MetricOvsSubsystemVswitchd,
	Name:      "interface_tx_errors_total",
	Help:      "The total number of transmitted packets with errors by Open vSwitch interface(s).",
})

var metricOvsInterfaceCollisionsTotal = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: MetricOvsNamespace,
	Subsystem: MetricOvsSubsystemVswitchd,
	Name:      "interface_collisions_total",
	Help:      "The total number of packet collisions transmitted by Open vSwitch interface(s).",
})

var metricOvsInterfaceTotal = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: MetricOvsNamespace,
	Subsystem: MetricOvsSubsystemVswitchd,
	Name:      "interfaces_total",
	Help:      "The total number of Open vSwitch interface(s) created for pods",
})

var MetricOvsInterfaceUpWait = prometheus.NewCounter(prometheus.CounterOpts{
	Namespace: MetricOvsNamespace,
	Subsystem: MetricOvsSubsystemVswitchd,
	Name:      "interface_up_wait_seconds_total",
	Help: "The total number of seconds that is required to wait for pod " +
		"Open vSwitch interface until its available",
})

// ovs memory metrics
var metricOvsHandlersTotal = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: MetricOvsNamespace,
	Subsystem: MetricOvsSubsystemVswitchd,
	Name:      "handlers_total",
	Help: "Represents the number of handlers thread. This thread reads upcalls from dpif, " +
		"forwards each upcall's packet and possibly sets up a kernel flow as a cache.",
})

var metricOvsRevalidatorsTotal = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: MetricOvsNamespace,
	Subsystem: MetricOvsSubsystemVswitchd,
	Name:      "revalidators_total",
	Help: "Represents the number of revalidators thread. This thread processes datapath flows, " +
		"updates OpenFlow statistics, and updates or removes them if necessary.",
})

// ovs Hw offload metrics
var metricOvsHwOffload = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: MetricOvsNamespace,
	Subsystem: MetricOvsSubsystemVswitchd,
	Name:      "hw_offload",
	Help: "Represents whether netdev flow offload to hardware is enabled " +
		"or not -- false(0) and true(1).",
})

var metricOvsTcPolicy = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: MetricOvsNamespace,
	Subsystem: MetricOvsSubsystemVswitchd,
	Name:      "tc_policy",
	Help: "Represents the policy used with HW offloading " +
		"-- none(0), skip_sw(1), and skip_hw(2).",
})

type ovsClient func(args ...string) (string, string, error)

func getOvsVersionInfo() {
	stdout, _, err := util.RunOVSVsctl("--version")
	if err == nil && strings.HasPrefix(stdout, "ovs-vsctl (Open vSwitch)") {
		ovsVersion = strings.Fields(stdout)[3]
	}
}

// ovsDatapathLookupsMetrics obtains the ovs datapath
// (lookups: hit, missed, lost) metrics and updates them.
func ovsDatapathLookupsMetrics(output, datapath string) {
	var datapathPacketsTotal float64
	for _, field := range strings.Fields(output) {
		elem := strings.Split(field, ":")
		if len(elem) != 2 {
			continue
		}
		switch elem[0] {
		case "hit":
			value := parseMetricToFloat(MetricOvsSubsystemVswitchd, "dp_flows_lookup_hit", elem[1])
			datapathPacketsTotal += value
			metricOvsDpFlowsLookupHit.WithLabelValues(datapath).Set(value)
		case "missed":
			value := parseMetricToFloat(MetricOvsSubsystemVswitchd, "dp_flows_lookup_missed", elem[1])
			datapathPacketsTotal += value
			metricOvsDpFlowsLookupMissed.WithLabelValues(datapath).Set(value)
		case "lost":
			value := parseMetricToFloat(MetricOvsSubsystemVswitchd, "dp_flows_lookup_lost", elem[1])
			metricOvsDpFlowsLookupLost.WithLabelValues(datapath).Set(value)
		}
	}
	metricOvsDpPacketsTotal.WithLabelValues(datapath).Set(datapathPacketsTotal)
}

// ovsDatapathMasksMetrics obatins ovs datapath masks metrics
// (masks :hit, total, hit/pkt) and updates them.
func ovsDatapathMasksMetrics(output, datapath string) {
	for _, field := range strings.Fields(output) {
		elem := strings.Split(field, ":")
		if len(elem) != 2 {
			continue
		}
		switch elem[0] {
		case "hit":
			value := parseMetricToFloat(MetricOvsSubsystemVswitchd, "dp_masks_hit", elem[1])
			metricOvsdpMasksHit.WithLabelValues(datapath).Set(value)
		case "total":
			value := parseMetricToFloat(MetricOvsSubsystemVswitchd, "dp_masks_total", elem[1])
			metricOvsDpMasksTotal.WithLabelValues(datapath).Set(value)
		case "hit/pkt":
			value := parseMetricToFloat(MetricOvsSubsystemVswitchd, "dp_masks_hit_ratio", elem[1])
			metricOvsDpMasksHitRatio.WithLabelValues(datapath).Set(value)
		}
	}
}

// getOvsDatapaths gives list of datapaths
// and updates the corresponding datapath metrics
func getOvsDatapaths(ovsAppctl ovsClient) (datapathsList []string, err error) {
	var stdout, stderr string

	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("recovering from a panic while parsing the "+
				"ovs-appctl dpctl/dump-dps output : %v", r)
		}
	}()

	stdout, stderr, err = ovsAppctl("dpctl/dump-dps")
	if err != nil {
		return nil, fmt.Errorf("failed to get output of ovs-appctl dpctl/dump-dps "+
			"stderr(%s) :(%v)", stderr, err)
	}
	for _, kvPair := range strings.Split(stdout, "\n") {
		var datapathType, datapathName string
		output := strings.TrimSpace(kvPair)
		if strings.Contains(output, "@") {
			datapath := strings.Split(output, "@")
			datapathType, datapathName = datapath[0], datapath[1]
		} else {
			return nil, fmt.Errorf("datapath %s is not of format Type@Name", output)
		}
		metricOvsDp.WithLabelValues(datapathName, datapathType).Set(1)
		datapathsList = append(datapathsList, datapathName)
	}
	metricOvsDpTotal.Set(float64(len(datapathsList)))
	return datapathsList, nil
}

func setOvsDatapathMetrics(ovsAppctl ovsClient, datapaths []string) (err error) {
	var stdout, stderr, datapathName string

	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("recovering from a panic while parsing the ovs-appctl dpctl/"+
				"show %s output : %v", datapathName, r)
		}
	}()

	for _, datapathName = range datapaths {
		stdout, stderr, err = ovsAppctl("dpctl/show", datapathName)
		if err != nil {
			return fmt.Errorf("failed to get datapath stats for %s "+
				"stderr(%s) :(%v)", datapathName, stderr, err)
		}
		var datapathPortCount float64
		for i, kvPair := range strings.Split(stdout, "\n") {
			if i <= 0 {
				// skip the first line which is datapath name
				continue
			}
			output := strings.TrimSpace(kvPair)
			if strings.HasPrefix(output, "lookups:") {
				ovsDatapathLookupsMetrics(output, datapathName)
			} else if strings.HasPrefix(output, "masks:") {
				ovsDatapathMasksMetrics(output, datapathName)
			} else if strings.HasPrefix(output, "port ") {
				datapathPortCount++
			} else if strings.HasPrefix(output, "flows:") {
				flowFields := strings.Fields(output)
				value := parseMetricToFloat(MetricOvsSubsystemVswitchd, "dp_flows_total", flowFields[1])
				metricOvsDpFlowsTotal.WithLabelValues(datapathName).Set(value)
			}
		}
		metricOvsDpIfTotal.WithLabelValues(datapathName).Set(datapathPortCount)
	}
	return nil
}

// ovsDatapathMetricsUpdater updates the ovs datapath metrics
func ovsDatapathMetricsUpdater(ovsAppctl ovsClient, tickPeriod time.Duration, stopChan <-chan struct{}) {
	ticker := time.NewTicker(tickPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			datapaths, err := getOvsDatapaths(ovsAppctl)
			if err != nil {
				klog.Errorf("Getting ovs datapath list failed: %s", err.Error())
				continue
			}
			if err = setOvsDatapathMetrics(ovsAppctl, datapaths); err != nil {
				klog.Errorf("Setting ovs datapath metrics failed: %s", err.Error())
			}
		case <-stopChan:
			return
		}
	}
}

// ovsBridgeMetricsUpdater updates bridge related metrics
func ovsBridgeMetricsUpdater(ovsVsctl, ovsAppctl ovsClient, tickPeriod time.Duration, stopChan <-chan struct{}) {
	ticker := time.NewTicker(tickPeriod)
	defer ticker.Stop()
	var err error
	for {
		select {
		case <-ticker.C:
			if err = updateOvsBridgeMetrics(ovsVsctl, ovsAppctl); err != nil {
				klog.Errorf("Getting ovs bridge info failed: %s", err.Error())
			}
		case <-stopChan:
			return
		}
	}
}

func updateOvsBridgeMetrics(ovsVsctl, ovsOfctl ovsClient) error {
	stdout, stderr, err := ovsVsctl("--no-headings", "--data=bare",
		"--format=csv", "--columns=name,port", "list", "Bridge")
	if err != nil {
		return fmt.Errorf("unable to update OVS bridge metrics due to failure to get output from"+
			" OVS client stderr(%s) :(%v)", stderr, err)
	}
	if stderr != "" {
		return fmt.Errorf("unable to update OVS bridge metrics because OVS client returned error: %s", stderr)
	}
	if stdout == "" {
		return fmt.Errorf("unable to update OVS bridge metrics because blank output received from OVS client")
	}

	//output will be of format :(br-local,12bc8575-8e1f-4583-b693-ea3b5bf09974
	// 5dc87c46-4d94-4469-9f7a-67ee1c8beb03 620cafe4-bfe5-4a23-8165-4ffc61e7de42)
	var bridgeCount int
	for _, kvPair := range strings.Split(stdout, "\n") {
		if kvPair == "" {
			continue
		}
		fields := strings.Split(kvPair, ",")
		bridgeName := fields[0]
		ports := strings.Fields(fields[1])
		if bridgeName != "" {
			bridgeCount++
			metricOvsBridge.WithLabelValues(bridgeName).Set(1)
			metricOvsBridgePortsTotal.WithLabelValues(bridgeName).Set(float64(len(ports)))
			count, err := getOvsBridgeOpenFlowsCount(ovsOfctl, bridgeName)
			if err != nil {
				return err
			}
			metricOvsBridgeFlowsTotal.WithLabelValues(bridgeName).Set(count)
		}
	}
	metricOvsBridgeTotal.Set(float64(bridgeCount))

	return nil
}

// getOvsBridgeOpenFlowsCount returns the number of openflow flows
// in an ovs-bridge
func getOvsBridgeOpenFlowsCount(ovsOfctl ovsClient, bridgeName string) (float64, error) {
	stdout, stderr, err := ovsOfctl("-t", "5", "dump-aggregate", bridgeName)
	if err != nil {
		return 0, fmt.Errorf("failed to get flow count for %s, stderr(%s): (%v)",
			bridgeName, stderr, err)
	}
	if stderr != "" {
		return 0, fmt.Errorf("failed to get OVS flow for %s due to stderr: %s", bridgeName, stderr)
	}
	if stdout == "" {
		return 0, fmt.Errorf("unable to update OVS bridge open flow count metric because blank output received from OVS client")
	}
	for _, kvPair := range strings.Fields(stdout) {
		if strings.HasPrefix(kvPair, "flow_count=") {
			value := strings.Split(kvPair, "=")[1]
			metricName := bridgeName + "flows_total"
			return parseMetricToFloat(MetricOvsSubsystemVswitchd, metricName, value), nil
		}
	}
	return 0, fmt.Errorf("ovs-ofctl dump-aggregate %s output didn't contain "+
		"flow_count field", bridgeName)
}

func ovsInterfaceMetricsUpdater(ovsVsctl ovsClient, tickPeriod time.Duration, stopChan <-chan struct{}) {
	ticker := time.NewTicker(tickPeriod)
	defer ticker.Stop()
	var err error
	for {
		select {
		case <-ticker.C:
			if err = updateOvsInterfaceMetrics(ovsVsctl); err != nil {
				klog.Errorf("Updating OVS interface metrics failed: %s", err.Error())
			}
		case <-stopChan:
			return
		}
	}
}

// updateOvsInterfaceMetrics updates the ovs interface metrics obtained from ovs-vsctl --columns=<fields> list interface
func updateOvsInterfaceMetrics(ovsVsctl ovsClient) error {
	var stdout, stderr string
	var err error

	stdout, stderr, err = ovsVsctl("--no-headings", "--data=bare",
		"--format=csv", "--columns=link_resets,statistics", "list", "Interface")
	if err != nil {
		return fmt.Errorf("failed to get output for ovs-vsctl list Interface "+
			"stderr(%s) :(%v)", stderr, err)
	}
	if stderr != "" {
		return fmt.Errorf("failed to get OVS interface metrics due to stderr: %s", stderr)
	}
	if stdout == "" {
		return fmt.Errorf("unable to update OVS interface metrics because blank output received from OVS client")
	}
	var linkReset, rxDropped, txDropped, rxErr, txErr, collisions, statValue, interfaceCount float64
	for _, kvPair := range strings.Split(stdout, "\n") {
		if kvPair == "" {
			continue
		}
		interfaceFieldValues := strings.Split(kvPair, ",")
		if len(interfaceFieldValues) != 2 {
			return fmt.Errorf("unexpected data format received while trying to get OVS interface metrics: %s", stdout)
		}
		statValue, err = strconv.ParseFloat(interfaceFieldValues[0], 64)
		if err != nil {
			return fmt.Errorf("expected string to contain an integer. Failed to get OVS interface metrics: %v", err)
		}
		linkReset += statValue
		interfaceCount++
		// sum statistics
		for _, field := range strings.Fields(interfaceFieldValues[1]) {
			statsField := strings.Split(field, "=")
			statName := strings.TrimSpace(statsField[0])
			statValue, err = strconv.ParseFloat(statsField[1], 64)
			if err != nil {
				return fmt.Errorf("expected string %q to contain an integer. Failed to get OVS interface metrics: %v",
					interfaceFieldValues[1], err)
			}

			switch statName {
			case "rx_dropped":
				rxDropped += statValue
			case "tx_dropped":
				txDropped += statValue
			case "rx_errors":
				rxErr += statValue
			case "tx_errors":
				txErr += statValue
			case "collisions":
				collisions += statValue
			}
		}
	}
	metricOvsInterfaceTotal.Set(interfaceCount)
	metricOvsInterfaceResetsTotal.Set(linkReset)
	metricOvsInterfaceRxDroppedTotal.Set(rxDropped)
	metricOvsInterfaceTxDroppedTotal.Set(txDropped)
	metricOvsInterfaceRxErrorsTotal.Set(rxErr)
	metricOvsInterfaceTxErrorsTotal.Set(txErr)
	metricOvsInterfaceCollisionsTotal.Set(collisions)
	return nil
}

// setOvsMemoryMetrics updates the handlers, revalidators
// count from "ovs-appctl -t ovs-vswitchd memory/show" output.
func setOvsMemoryMetrics(ovsVswitchdAppctl ovsClient) (err error) {
	var stdout, stderr string

	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("recovering from panic while parsing the ovs-appctl "+
				"memory/show output : %v", r)
		}
	}()

	stdout, stderr, err = ovsVswitchdAppctl("memory/show")
	if err != nil {
		return fmt.Errorf("failed to retrieve memory/show output "+
			"for ovs-vswitchd stderr(%s) :%v", stderr, err)
	}

	for _, kvPair := range strings.Fields(stdout) {
		if strings.HasPrefix(kvPair, "handlers:") {
			value := strings.Split(kvPair, ":")[1]
			count := parseMetricToFloat(MetricOvsSubsystemVswitchd, "handlers_total", value)
			metricOvsHandlersTotal.Set(count)
		} else if strings.HasPrefix(kvPair, "revalidators:") {
			value := strings.Split(kvPair, ":")[1]
			count := parseMetricToFloat(MetricOvsSubsystemVswitchd, "revalidators_total", value)
			metricOvsRevalidatorsTotal.Set(count)
		}
	}
	return nil
}

func ovsMemoryMetricsUpdater(ovsVswitchdAppctl ovsClient, tickPeriod time.Duration, stopChan <-chan struct{}) {
	ticker := time.NewTicker(tickPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := setOvsMemoryMetrics(ovsVswitchdAppctl); err != nil {
				klog.Errorf("Setting ovs memory metrics failed: %s", err.Error())
			}
		case <-stopChan:
			return
		}
	}
}

// setOvsHwOffloadMetrics obatains the hw-offlaod, tc-policy
// ovs-vsctl list Open_vSwitch . and updates the corresponding metrics
func setOvsHwOffloadMetrics(ovsVsctl ovsClient) (err error) {
	var stdout, stderr string

	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("recovering from panic while parsing the ovs-vsctl "+
				"list Open_vSwitch . output : %v", r)
		}
	}()

	stdout, stderr, err = ovsVsctl("--no-headings", "--data=bare",
		"--columns=other_config", "list", "Open_vSwitch", ".")
	if err != nil {
		return fmt.Errorf("failed to get output from ovs-vsctl list --columns=other_config"+
			"open_vSwitch . stderr(%s) : %v", stderr, err)
	}

	var hwOffloadValue = "false"
	var tcPolicyValue = "none"
	var tcPolicyMap = map[string]float64{
		"none":    0,
		"skip_sw": 1,
		"skip_hw": 2,
	}
	for _, kvPair := range strings.Fields(stdout) {
		if strings.HasPrefix(kvPair, "hw-offload=") {
			hwOffloadValue = strings.Split(kvPair, "=")[1]
		} else if strings.HasPrefix(kvPair, "tc-policy=") {
			tcPolicyValue = strings.Split(kvPair, "=")[1]
		}
	}

	if hwOffloadValue == "false" {
		metricOvsHwOffload.Set(0)
	} else {
		metricOvsHwOffload.Set(1)
	}
	metricOvsTcPolicy.Set(tcPolicyMap[tcPolicyValue])
	return nil
}

func ovsHwOffloadMetricsUpdater(ovsVsctl ovsClient, tickPeriod time.Duration, stopChan <-chan struct{}) {
	ticker := time.NewTicker(tickPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := setOvsHwOffloadMetrics(ovsVsctl); err != nil {
				klog.Errorf("Setting ovs hardware offload metrics failed: %s", err.Error())
			}
		case <-stopChan:
			return
		}
	}
}

var ovsVswitchdCoverageShowMetricsMap = map[string]*metricDetails{
	"netlink_sent": {
		help: "Number of netlink message sent to the kernel.",
	},
	"netlink_received": {
		help: "Number of netlink messages received by the kernel.",
	},
	"netlink_recv_jumbo": {
		help: "Number of netlink messages that were received from" +
			"the kernel were more than the allocated buffer.",
	},
	"netlink_overflow": {
		help: "Netlink messages dropped by the daemon due " +
			"to buffer overflow.",
	},
	"rconn_sent": {
		help: "Specifies the number of messages " +
			"that have been sent to the underlying virtual " +
			"connection (unix, tcp, or ssl) to OpenFlow devices.",
	},
	"rconn_queued": {
		help: "Specifies the number of messages that have been " +
			"queued because it couldn’t be sent using the " +
			"underlying virtual connection to OpenFlow devices.",
	},
	"rconn_discarded": {
		help: "Specifies the number of messages that " +
			"have been dropped because the send queue " +
			"had to be flushed because of reconnection.",
	},
	"rconn_overflow": {
		help: "Specifies the number of messages that have " +
			"been dropped because of the queue overflow.",
	},
	"vconn_open": {
		help: "Specifies the number of attempts to connect " +
			"to an OpenFlow Device.",
	},
	"vconn_sent": {
		help: "Specifies the number of messages sent " +
			"to the OpenFlow Device.",
	},
	"vconn_received": {
		help: "Specifies the number of messages received " +
			"from the OpenFlow Device.",
	},
	"pstream_open": {
		help: "Specifies the number of time passive connections " +
			"were opened for the remote peer to connect.",
	},
	"stream_open": {
		help: "Specifies the number of attempts to connect " +
			"to a remote peer (active connection).",
	},
	"txn_success": {
		help: "Specifies the number of times the OVSDB " +
			"transaction has successfully completed.",
	},
	"txn_error": {
		help: "Specifies the number of times the OVSDB " +
			"transaction has errored out.",
	},
	"txn_uncommitted": {
		help: "Specifies the number of times the OVSDB " +
			"transaction were uncommitted.",
	},
	"txn_unchanged": {
		help: "Specifies the number of times the OVSDB transaction " +
			"resulted in no change to the database.",
	},
	"txn_incomplete": {
		help: "Specifies the number of times the OVSDB transaction " +
			"did not complete and the client had to re-try.",
	},
	"txn_aborted": {
		help: "Specifies the number of times the OVSDB " +
			" transaction has been aborted.",
	},
	"txn_try_again": {
		help: "Specifies the number of times the OVSDB " +
			"transaction failed and the client had to re-try.",
	},
	"dpif_port_add": {
		help: "Number of times a netdev was added as a port to the dpif.",
	},
	"dpif_port_del": {
		help: "Number of times a netdev was removed from the dpif.",
	},
	"dpif_flow_flush": {
		help: "Number of times flows were flushed from the datapath " +
			"(Linux kernel datapath module).",
	},
	"dpif_flow_get": {
		help: "Number of times flows were retrieved from the " +
			"datapath (Linux kernel datapath module).",
	},
	"dpif_flow_put": {
		help: "Number of times flows were added to the datapath " +
			"(Linux kernel datapath module).",
	},
	"dpif_flow_del": {
		help: "Number of times flows were deleted from the " +
			"datapath (Linux kernel datapath module).",
	},
	"dpif_execute": {
		aggregateFrom: []string{
			"dpif_execute",
			"dpif_execute_with_help",
		},
		help: "Number of times the OpenFlow actions were executed in userspace " +
			"on behalf of the datapath.",
	},
	"bridge_reconfigure": {
		help: "Number of times OVS bridges were reconfigured.",
	},
	"xlate_actions": {
		help: "Number of times an OpenFlow actions were translated " +
			"into datapath actions.",
	},
	"xlate_actions_oversize": {
		help: "Number of times the translated OpenFlow actions into " +
			"a datapath actions were too big for a netlink attribute.",
	},
	"xlate_actions_too_many_output": {
		help: "Number of times the number of datapath actions " +
			"were more than what the kernel can handle reliably.",
	},
	"packet_in": {
		srcName: "flow_extract",
		help: "Specifies the number of times ovs-vswitchd has " +
			"handled the packet-ins on behalf of kernel datapath.",
	},
	"packet_in_drop": {
		srcName: "packet_in_overflow",
		help: "Specifies the number of times the ovs-vswitchd has dropped the " +
			"packet-ins due to resource constraints.",
	},
	"ofproto_dpif_expired": {
		help: "Number of times the flows were removed for reasons - " +
			"idle timeout, hard timeout, flow delete,  group delete, " +
			"meter delete, or eviction.",
	},
	"ofproto_flush": {
		help: "Number of times the flows from all of ofproto's " +
			"flow tables were flushed.",
	},
	"ofproto_packet_out": {
		help: "Number of times the controller injected the packet " +
			"into the kernel datapath.",
	},
	"ofproto_recv_openflow": {
		help: "Number of times an OpenFlow message was handled.",
	},
	"ofproto_reinit_ports": {
		help: "Number of times all the OpenFlow ports were reinitialized.",
	},
}
var registerOvsMetricsOnce sync.Once

func RegisterStandaloneOvsMetrics(stopChan <-chan struct{}) {
	registerOvsMetrics(prometheus.DefaultRegisterer, stopChan)
}

func RegisterOvsMetricsWithOvnMetrics(stopChan <-chan struct{}) {
	registerOvsMetrics(ovnRegistry, stopChan)
}

func registerOvsMetrics(registry prometheus.Registerer, stopChan <-chan struct{}) {
	registerOvsMetricsOnce.Do(func() {
		getOvsVersionInfo()
		registry.MustRegister(prometheus.NewGaugeFunc(
			prometheus.GaugeOpts{
				Namespace: MetricOvsNamespace,
				Name:      "build_info",
				Help:      "A metric with a constant '1' value labeled by ovs version.",
				ConstLabels: prometheus.Labels{
					"version": ovsVersion,
				},
			},
			func() float64 { return 1 },
		))

		// Register OVS datapath metrics.
		registry.MustRegister(metricOvsDpTotal)
		registry.MustRegister(metricOvsDp)
		registry.MustRegister(metricOvsDpIfTotal)
		registry.MustRegister(metricOvsDpFlowsTotal)
		registry.MustRegister(metricOvsDpFlowsLookupHit)
		registry.MustRegister(metricOvsDpFlowsLookupMissed)
		registry.MustRegister(metricOvsDpFlowsLookupLost)
		registry.MustRegister(metricOvsDpPacketsTotal)
		registry.MustRegister(metricOvsdpMasksHit)
		registry.MustRegister(metricOvsDpMasksTotal)
		registry.MustRegister(metricOvsDpMasksHitRatio)
		// Register OVS bridge statistics & attributes metrics
		registry.MustRegister(metricOvsBridgeTotal)
		registry.MustRegister(metricOvsBridge)
		registry.MustRegister(metricOvsBridgePortsTotal)
		registry.MustRegister(metricOvsBridgeFlowsTotal)
		// Register ovs Memory metrics
		registry.MustRegister(metricOvsHandlersTotal)
		registry.MustRegister(metricOvsRevalidatorsTotal)
		// Register OVS HW offload metrics
		registry.MustRegister(metricOvsHwOffload)
		registry.MustRegister(metricOvsTcPolicy)
		// Register OVS Interface metrics
		registry.MustRegister(metricOvsInterfaceResetsTotal)
		registry.MustRegister(metricOvsInterfaceRxDroppedTotal)
		registry.MustRegister(metricOvsInterfaceTxDroppedTotal)
		registry.MustRegister(metricOvsInterfaceRxErrorsTotal)
		registry.MustRegister(metricOvsInterfaceTxErrorsTotal)
		registry.MustRegister(metricOvsInterfaceCollisionsTotal)
		registry.MustRegister(metricOvsInterfaceTotal)
		registry.MustRegister(MetricOvsInterfaceUpWait)
		// Register the OVS coverage/show metrics
		componentCoverageShowMetricsMap[ovsVswitchd] = ovsVswitchdCoverageShowMetricsMap
		registerCoverageShowMetrics(ovsVswitchd, MetricOvsNamespace, MetricOvsSubsystemVswitchd)

		registry.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{
			PidFn:     prometheus.NewPidFileFn("/var/run/openvswitch/ovs-vswitchd.pid"),
			Namespace: fmt.Sprintf("%s_%s", MetricOvsNamespace, MetricOvsSubsystemVswitchd),
		}))
		registry.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{
			PidFn:     prometheus.NewPidFileFn("/var/run/openvswitch/ovsdb-server.pid"),
			Namespace: fmt.Sprintf("%s_%s", MetricOvsNamespace, MetricOvsSubsystemDB),
		}))
		// OVS datapath metrics updater
		go ovsDatapathMetricsUpdater(util.RunOVSAppctl, 30*time.Second, stopChan)
		// OVS bridge metrics updater
		go ovsBridgeMetricsUpdater(util.RunOVSVsctl, util.RunOVSOfctl, 30*time.Second, stopChan)
		// OVS interface metrics updater
		go ovsInterfaceMetricsUpdater(util.RunOVSVsctl, 30*time.Second, stopChan)
		// OVS memory metrics updater
		go ovsMemoryMetricsUpdater(util.RunOvsVswitchdAppCtl, 30*time.Second, stopChan)
		// OVS hw Offload metrics updater
		go ovsHwOffloadMetricsUpdater(util.RunOVSVsctl, 30*time.Second, stopChan)
		// OVS coverage/show metrics updater.
		go coverageShowMetricsUpdater(ovsVswitchd, stopChan)
	})
}
