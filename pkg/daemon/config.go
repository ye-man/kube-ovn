package daemon

import (
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/alauda/kube-ovn/pkg/util"
	"github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
	"github.com/vishvananda/netlink"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog"
)

// Configuration is the daemon conf
type Configuration struct {
	Iface                 string
	MTU                   int
	EnableMirror          bool
	MirrorNic             string
	BindSocket            string
	OvsSocket             string
	KubeConfigFile        string
	KubeClient            kubernetes.Interface
	NodeName              string
	ServiceClusterIPRange string
	PprofPort             int
}

// ParseFlags will parse cmd args then init kubeClient and configuration
// TODO: validate configuration
func ParseFlags() (*Configuration, error) {
	var (
		argIface                 = pflag.String("iface", "", "The iface used to inter-host pod communication, default: the default route iface")
		argMTU                   = pflag.Int("mtu", 0, "The MTU used by pod iface, default: iface MTU - 55")
		argEnableMirror          = pflag.Bool("enable-mirror", false, "Enable traffic mirror, default: false")
		argMirrorNic             = pflag.String("mirror-iface", "mirror0", "The mirror nic name that will be created by kube-ovn, default: mirror0")
		argBindSocket            = pflag.String("bind-socket", "/var/run/cniserver.sock", "The socket daemon bind to.")
		argOvsSocket             = pflag.String("ovs-socket", "", "The socket to local ovs-server")
		argKubeConfigFile        = pflag.String("kubeconfig", "", "Path to kubeconfig file with authorization and master location information. If not set use the inCluster token.")
		argServiceClusterIPRange = pflag.String("service-cluster-ip-range", "10.96.0.0/12", "The kubernetes service cluster ip range, default: 10.96.0.0/12")
		argPprofPort             = pflag.Int("pprof-port", 10665, "The port to get profiling data, default: 10665")
	)

	// mute log for ipset lib
	logrus.SetOutput(ioutil.Discard)

	flag.Set("alsologtostderr", "true")

	klogFlags := flag.NewFlagSet("klog", flag.ExitOnError)
	klog.InitFlags(klogFlags)

	// Sync the glog and klog flags.
	flag.CommandLine.VisitAll(func(f1 *flag.Flag) {
		f2 := klogFlags.Lookup(f1.Name)
		if f2 != nil {
			value := f1.Value.String()
			f2.Value.Set(value)
		}
	})

	pflag.CommandLine.AddGoFlagSet(klogFlags)
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)
	pflag.Parse()

	nodeName := os.Getenv("KUBE_NODE_NAME")
	if nodeName == "" {
		klog.Errorf("env KUBE_NODE_NAME not exists")
		return nil, fmt.Errorf("env KUBE_NODE_NAME not exists")
	}

	config := &Configuration{
		Iface:                 *argIface,
		MTU:                   *argMTU,
		EnableMirror:          *argEnableMirror,
		MirrorNic:             *argMirrorNic,
		BindSocket:            *argBindSocket,
		OvsSocket:             *argOvsSocket,
		KubeConfigFile:        *argKubeConfigFile,
		PprofPort:             *argPprofPort,
		NodeName:              nodeName,
		ServiceClusterIPRange: *argServiceClusterIPRange,
	}

	if config.Iface == "" {
		iface, err := getDefaultGatewayIface()
		if err != nil {
			return nil, err
		} else {
			config.Iface = iface
		}
	}
	iface, err := net.InterfaceByName(config.Iface)
	if err != nil {
		return nil, err
	}
	if config.MTU == 0 {
		config.MTU = iface.MTU - util.GeneveHeaderLength
	}

	addrs, err := iface.Addrs()
	if err != nil {
		return nil, fmt.Errorf("failed to get iface addr. %v", err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("iface %s has no ip address", config.Iface)
	}
	if err := setEncapIP(strings.Split(addrs[0].String(), "/")[0]); err != nil {
		return nil, err
	}

	err = config.initKubeClient()
	if err != nil {
		return nil, err
	}
	klog.Infof("daemon config: %v", config)
	return config, nil
}

func (config *Configuration) initKubeClient() error {
	var cfg *rest.Config
	var err error
	if config.KubeConfigFile == "" {
		klog.Infof("no --kubeconfig, use in-cluster kubernetes config")
		cfg, err = rest.InClusterConfig()
		if err != nil {
			klog.Errorf("use in cluster config failed %v", err)
			return err
		}
	} else {
		cfg, err = clientcmd.BuildConfigFromFlags("", config.KubeConfigFile)
		if err != nil {
			klog.Errorf("use --kubeconfig %s failed %v", config.KubeConfigFile, err)
			return err
		}
	}
	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		klog.Errorf("init kubernetes client failed %v", err)
		return err
	}

	config.KubeClient = kubeClient
	return nil
}

func getDefaultGatewayIface() (string, error) {
	routes, err := netlink.RouteList(nil, syscall.AF_INET)
	if err != nil {
		return "", err
	}

	for _, route := range routes {
		if route.Dst == nil || route.Dst.String() == "0.0.0.0/0" {
			if route.LinkIndex <= 0 {
				return "", errors.New("found default route but could not determine interface")
			}
			iface, err := net.InterfaceByIndex(route.LinkIndex)
			if err != nil {
				return "", fmt.Errorf("failed to get iface %v", err)
			}
			return iface.Name, nil
		}
	}

	return "", errors.New("unable to find default route")
}

func setEncapIP(ip string) error {
	raw, err := exec.Command(
		"ovs-vsctl", "set", "open", ".", fmt.Sprintf("external-ids:ovn-encap-ip=%s", ip)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to set ovn-encap-ip, %s", string(raw))
	}
	return nil
}
