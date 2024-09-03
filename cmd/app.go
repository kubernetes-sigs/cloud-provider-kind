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
	"sigs.k8s.io/cloud-provider-kind/pkg/controller"
	"sigs.k8s.io/kind/pkg/cluster"
	kindcmd "sigs.k8s.io/kind/pkg/cmd"
)

var (
	flagV               int
	enableLogDump       bool
	logDumpDir          string
	enableLBPortMapping bool
)

func init() {
	flag.IntVar(&flagV, "v", 2, "Verbosity level")
	flag.BoolVar(&enableLogDump, "enable-log-dumping", false, "store logs to a temporal directory or to the directory specified using the logs-dir flag")
	flag.StringVar(&logDumpDir, "logs-dir", "", "store logs to the specified directory")
	flag.BoolVar(&enableLBPortMapping, "enable-lb-port-mapping", false, "enable port-mapping on the load balancer ports")

	flag.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: cloud-provider-kind [options]\n\n")
		flag.PrintDefaults()
	}
}

func Main() {
	// Parse command line flags and arguments
	flag.Parse()
	flag.VisitAll(func(flag *flag.Flag) {
		klog.Infof("FLAG: --%s=%q", flag.Name, flag.Value)
	})

	// Process on macOS must run using sudo
	if runtime.GOOS == "darwin" && syscall.Geteuid() != 0 {
		klog.Warning("Please run this again with `sudo`.")
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

func isWSL2() bool {
	if v, err := os.ReadFile("/proc/version"); err == nil {
		return strings.Contains(string(v), "WSL2")
	}

	return false
}
