package cmd

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"

	"github.com/go-logr/logr"
	"k8s.io/component-base/logs"
	"k8s.io/klog/v2"

	"sigs.k8s.io/cloud-provider-kind/pkg/config"
	"sigs.k8s.io/cloud-provider-kind/pkg/controller"
	"sigs.k8s.io/cloud-provider-kind/pkg/images"
	"sigs.k8s.io/kind/pkg/cluster"
	kindcmd "sigs.k8s.io/kind/pkg/cmd"
)

var (
	enableLogDump        bool
	logDumpDir           string
	enableLBPortMapping  bool
	gatewayChannel       string
	enableDefaultIngress bool
)

func init() {
	subcommands := [][]string{
		{"list-images", "list images used by cloud-provider-kind"},
	}

	klog.InitFlags(flag.CommandLine)
	_ = flag.Set("v", "2")
	flag.BoolVar(&enableLogDump, "enable-log-dumping", false, "store logs to a temporal directory or to the directory specified using the logs-dir flag")
	flag.StringVar(&logDumpDir, "logs-dir", "", "store logs to the specified directory")
	flag.BoolVar(&enableLBPortMapping, "enable-lb-port-mapping", false, "enable port-mapping on the load balancer ports")
	flag.StringVar(&gatewayChannel, "gateway-channel", "standard", "define the gateway API release channel to be used (standard,experimental), by default is standard")
	flag.BoolVar(&enableDefaultIngress, "enable-default-ingress", true, "enable default ingress for the cloud provider kind ingress")

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
	// Parse subcommands if exist
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "list-images":
			listImages()
			return
		default:
		}
	}

	// Parse command line flags and arguments
	flag.Parse()
	flag.VisitAll(func(flag *flag.Flag) {
		klog.Infof("FLAG: --%s=%q", flag.Name, flag.Value)
	})

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
	logger := klog.NewKlogr()
	ctx = logr.NewContext(ctx, logger)

	kindLogger := kindcmd.NewLogger()
	type verboser interface {
		SetVerbosity(int)
	}
	v, ok := kindLogger.(verboser)
	if ok {
		v.SetVerbosity(logger.GetV())
	}

	_, err := logs.GlogSetter(fmt.Sprintf("%d", logger.GetV()))
	if err != nil {
		logger.Error(err, "Error setting klog verbosity")
	}

	config.DefaultConfig.IngressDefault = enableDefaultIngress

	if config.GatewayReleaseChannel(gatewayChannel) == "" {
		klog.Fatalf("Unknown Gateway API release channel %s", gatewayChannel)
	}
	config.DefaultConfig.GatewayReleaseChannel = config.GatewayReleaseChannel(gatewayChannel)

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
				logger.Error(err, "Directory does not exist", "path", logDumpDir)
				klog.FlushAndExit(klog.ExitFlushTimeout, 1)
			}
		}
		config.DefaultConfig.EnableLogDump = true
		config.DefaultConfig.LogDir = logDumpDir
		logger.Info("**** Dumping load balancers", "path", logDumpDir)
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
		logger.Error(err, "Unable to detect cluster provider")
		klog.FlushAndExit(klog.ExitFlushTimeout, 1)
	}
	kindProvider := cluster.NewProvider(
		option,
		cluster.ProviderWithLogger(kindLogger),
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
	for _, img := range images.Images {
		fmt.Println(img)
	}
}

func isWSL2() bool {
	if v, err := os.ReadFile("/proc/version"); err == nil {
		return strings.Contains(string(v), "WSL2")
	}

	return false
}
