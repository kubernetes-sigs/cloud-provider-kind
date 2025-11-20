package cmd

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"k8s.io/component-base/logs"
	"k8s.io/klog/v2"

	"sigs.k8s.io/cloud-provider-kind/pkg/config"
	"sigs.k8s.io/cloud-provider-kind/pkg/constants"
	"sigs.k8s.io/cloud-provider-kind/pkg/controller"
	"sigs.k8s.io/kind/pkg/cluster"
	kindcmd "sigs.k8s.io/kind/pkg/cmd"
)

var (
	flagV                int
	enableLogDump        bool
	logDumpDir           string
	enableLBPortMapping  bool
	gatewayChannel       string
	enableDefaultIngress bool
	proxyImage           string
)

func init() {
	subcommands := [][]string{
		{"list-images", "list images used by cloud-provider-kind"},
	}

	flag.IntVar(&flagV, "v", 2, "Verbosity level")
	flag.BoolVar(&enableLogDump, "enable-log-dumping", false, "store logs to a temporal directory or to the directory specified using the logs-dir flag")
	flag.StringVar(&logDumpDir, "logs-dir", "", "store logs to the specified directory")
	flag.BoolVar(&enableLBPortMapping, "enable-lb-port-mapping", false, "enable port-mapping on the load balancer ports")
	flag.StringVar(&gatewayChannel, "gateway-channel", "standard", "define the gateway API release channel to be used (standard, experimental, disabled), by default is standard")
	flag.BoolVar(&enableDefaultIngress, "enable-default-ingress", true, "enable default ingress for the cloud provider kind ingress")
	flag.StringVar(&proxyImage, "proxy-image", constants.DefaultProxyImage, "proxy image to use for load balancers")

	flag.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: cloud-provider-kind [subcommand] [options]\n\n")
		fmt.Fprintln(os.Stderr, "Subcommands:")
		printSubcommands(subcommands)
		fmt.Fprint(os.Stderr, "\n")
		fmt.Fprintln(os.Stderr, "Options:")
		flag.PrintDefaults()
	}
}

func Main() {
	// Parse command line flags and arguments
	flag.Parse()
	flag.VisitAll(func(flag *flag.Flag) {
		klog.Infof("FLAG: --%s=%q", flag.Name, flag.Value)
	})

	// Validate Proxy Image
	if proxyImage == "" {
		klog.Fatalf("proxy image can not be empty")
	}
	config.DefaultConfig.ProxyImage = proxyImage

	// Parse subcommands if exist
	if len(flag.Args()) > 0 {
		switch flag.Args()[0] {
		case "list-images":
			listImages()
			return
		default:
		}
	}

	// don't allow subcommands after flags
	if len(flag.Args()) > 0 {
		flag.Usage()
		os.Exit(1)
	}

	// Process on macOS must run using sudo
	if runtime.GOOS == "darwin" && syscall.Geteuid() != 0 {
		klog.Fatalf("Please run this again with `sudo`.")
	}

	// trap Ctrl+C and call cancel on the context
	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)

	// Enable signal handler
	signalCh := make(chan os.Signal, 2)
	defer func() {
		close(signalCh)
		cancel()
	}()

	signal.Notify(signalCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		select {
		case <-signalCh:
			klog.Infof("Exiting: received signal")
			cancel()
		case <-ctx.Done():
			// cleanup
		}
	}()

	// initialize loggers, kind logger and klog
	logger := kindcmd.NewLogger()
	type verboser interface {
		SetVerbosity(int)
	}
	v, ok := logger.(verboser)
	if ok {
		v.SetVerbosity(flagV)
	}

	_, err := logs.GlogSetter(strconv.Itoa(flagV))
	if err != nil {
		logger.Errorf("error setting klog verbosity to %d : %v", flagV, err)
	}

	config.DefaultConfig.IngressDefault = enableDefaultIngress

	// Validate gateway channel
	channel := config.GatewayReleaseChannel(gatewayChannel)
	if channel != config.Standard && channel != config.Experimental && channel != config.Disabled {
		klog.Fatalf("Unknown Gateway API release channel %q", gatewayChannel)
	}
	config.DefaultConfig.GatewayReleaseChannel = channel

	// initialize log directory
	if enableLogDump {
		if logDumpDir == "" {
			dir, err := os.MkdirTemp(os.TempDir(), "kind-provider-")
			if err != nil {
				klog.Fatal(err)
			}
			logDumpDir = dir
		}

		if _, err := os.Stat(logDumpDir); os.IsNotExist(err) {
			if err := os.MkdirAll(logDumpDir, 0755); err != nil {
				klog.Fatalf("directory %s does not exist: %v", logDumpDir, err)
			}
		}
		config.DefaultConfig.EnableLogDump = true
		config.DefaultConfig.LogDir = logDumpDir
		klog.Infof("**** Dumping load balancers logs to: %s", logDumpDir)
	}

	// some platforms require to enable tunneling for the LoadBalancers
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" || isWSL2() {
		config.DefaultConfig.LoadBalancerConnectivity = config.Tunnel
	}

	// flag overrides autodetection
	if enableLBPortMapping {
		config.DefaultConfig.LoadBalancerConnectivity = config.Portmap
	}

	// default control plane connectivity to portmap, it will be
	// overriden if the first cluster added detects direct
	// connecitivity
	config.DefaultConfig.ControlPlaneConnectivity = config.Portmap

	// initialize kind provider
	option, err := cluster.DetectNodeProvider()
	if err != nil {
		klog.Fatalf("can not detect cluster provider: %v", err)
	}
	kindProvider := cluster.NewProvider(
		option,
		cluster.ProviderWithLogger(logger),
	)
	controller.New(kindProvider).Run(ctx)
}

func printSubcommands(subcommands [][]string) {
	for _, subcmd := range subcommands {
		fmt.Fprintf(os.Stderr, "  %s\n", subcmd[0])
		fmt.Fprintf(os.Stderr, "        %s\n", subcmd[1])
	}
}

func listImages() {
	fmt.Println(config.DefaultConfig.ProxyImage)
}

func isWSL2() bool {
	if v, err := os.ReadFile("/proc/version"); err == nil {
		return strings.Contains(string(v), "WSL2")
	}

	return false
}
