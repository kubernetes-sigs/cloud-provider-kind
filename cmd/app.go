package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"k8s.io/component-base/logs"
	"k8s.io/klog/v2"

	"sigs.k8s.io/cloud-provider-kind/pkg/config"
	"sigs.k8s.io/cloud-provider-kind/pkg/container"
	"sigs.k8s.io/cloud-provider-kind/pkg/controller"
	"sigs.k8s.io/kind/pkg/cluster"
	kindcmd "sigs.k8s.io/kind/pkg/cmd"
)

var (
	flagV                int
	enableLogDump        bool
	logDumpDir           string
	enableLBPortMapping  bool
	enableLBTunnel       bool
	gatewayChannel       string
	enableDefaultIngress bool
	version              string
)

func Main() {
	cmd := NewCommand()
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func NewCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cloud-provider-kind",
		Short: "cloud-provider-kind is a cloud provider for kind clusters",
		RunE:  runE,
		// We silence usage on error because we don't want to print usage
		// when the command fails due to a runtime error.
		SilenceUsage: true,
	}

	// Register flags
	cmd.Flags().IntVarP(&flagV, "verbosity", "v", 2, "Verbosity level")
	cmd.Flags().BoolVar(&enableLogDump, "enable-log-dumping", false, "store logs to a temporal directory or to the directory specified using the logs-dir flag")
	cmd.Flags().StringVar(&logDumpDir, "logs-dir", "", "store logs to the specified directory")
	cmd.Flags().BoolVar(&enableLBPortMapping, "enable-lb-port-mapping", false, "enable port-mapping on the load balancer ports")
	if (runtime.GOOS == "linux") {
		cmd.Flags().BoolVar(&enableLBTunnel, "enable-lb-tunnel", false, "enable tunneling load balancer ips")
	}
	cmd.Flags().StringVar(&gatewayChannel, "gateway-channel", "standard", "define the gateway API release channel to be used (standard, experimental, disabled), by default is standard")
	cmd.Flags().BoolVar(&enableDefaultIngress, "enable-default-ingress", true, "enable default ingress for the cloud provider kind ingress")


	cmd.AddCommand(newListImagesCommand())
	cmd.AddCommand(newVersionCommand())

	return cmd
}

func newListImagesCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list-images",
		Short: "list images used by cloud-provider-kind",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println(config.DefaultConfig.ProxyImage)
		},
	}
}

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use: "version",
		Short: "print the cloud-provider-kind version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println("cloud-provider-kind", version)
		},
	}
}

func runE(cmd *cobra.Command, args []string) error {
	// Log flags
	var disableAutoDetect bool
	cmd.Flags().VisitAll(func(flag *pflag.Flag) {
		klog.Infof("FLAG: --%s=%q", flag.Name, flag.Value)
		// presence of these flags override auto-detection
		if flag.Name == "enable-lb-port-mapping" || flag.Name == "enable-lb-tunnel" {
			if flag.Changed {
				disableAutoDetect = true
			}
		}
	})

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
		return fmt.Errorf("unknown Gateway API release channel %q", gatewayChannel)
	}
	config.DefaultConfig.GatewayReleaseChannel = channel

	// initialize log directory
	if enableLogDump {
		if logDumpDir == "" {
			dir, err := os.MkdirTemp(os.TempDir(), "kind-provider-")
			if err != nil {
				return err
			}
			logDumpDir = dir
		}

		if _, err := os.Stat(logDumpDir); os.IsNotExist(err) {
			if err := os.MkdirAll(logDumpDir, 0755); err != nil {
				return fmt.Errorf("directory %s does not exist: %v", logDumpDir, err)
			}
		}
		config.DefaultConfig.EnableLogDump = true
		config.DefaultConfig.LogDir = logDumpDir
		klog.Infof("**** Dumping load balancers logs to: %s", logDumpDir)
	}

	// default control plane connectivity to portmap, it will be
	// overriden if the first cluster added detects direct
	// connecitivity
	config.DefaultConfig.ControlPlaneConnectivity = config.Portmap

	// initialize kind provider
	// Check KIND_EXPERIMENTAL_PROVIDER directly, as the kind CLI does, so it
	// takes effect regardless of auto-detection results.
	var option cluster.ProviderOption
	if p := os.Getenv("KIND_EXPERIMENTAL_PROVIDER"); p != "" {
		switch p {
		case "podman":
			klog.Infof("Using podman provider due to KIND_EXPERIMENTAL_PROVIDER")
			option = cluster.ProviderWithPodman()
			container.SetRuntime("podman")
		case "docker":
			klog.Infof("Using docker provider due to KIND_EXPERIMENTAL_PROVIDER")
			option = cluster.ProviderWithDocker()
			container.SetRuntime("docker")
		case "nerdctl", "finch", "nerdctl.lima":
			klog.Infof("Using %s provider due to KIND_EXPERIMENTAL_PROVIDER", p)
			option = cluster.ProviderWithNerdctl(p)
			container.SetRuntime(p)
		default:
			klog.Warningf("Ignoring unknown KIND_EXPERIMENTAL_PROVIDER value %q", p)
		}
	}
	if option == nil {
		p, err := container.DetectRuntime()
		if err != nil {
			return err
		}

		klog.Infof("Detect runtime: %s", p)
		container.SetRuntime(p)
		switch p {
		case "podman":
			option = cluster.ProviderWithPodman()
		case "nerdctl", "finch", "nerdctl.lima":
			option = cluster.ProviderWithNerdctl(p)
		default:
			option = cluster.ProviderWithDocker()
		}
	}

	// any --enable-lb-* flags (true or false) override lb-tunnel auto-detection.
	//nolint:gocritic // if-else chain is more readable than switch
	if enableLBTunnel {
		config.DefaultConfig.LoadBalancerConnectivity = config.Tunnel
	} else if enableLBPortMapping {
		config.DefaultConfig.LoadBalancerConnectivity = config.Portmap
	} else if ! disableAutoDetect {
		// auto-detect lb-tunnel
		if (runtime.GOOS == "linux" && syscall.Geteuid() == 0) {
			// linux run as sudo
			if isRootless, err := container.DetectRootless(); err != nil {
				klog.Infof("Failed to auto-detect lb-tunnel mode: %s", err)
			} else if isRootless {
				// if containers are rootless, choose tunnel mode
				klog.Infof("Containers are rootless; using lb-tunnel mode")
				config.DefaultConfig.LoadBalancerConnectivity = config.Tunnel
			}
		}

		// some platforms default to tunneling
		if runtime.GOOS == "darwin" || runtime.GOOS == "windows" || isWSL2() {
			klog.Infof("Containers are remote; using lb-tunnel mode")
			config.DefaultConfig.LoadBalancerConnectivity = config.Tunnel
		}
	}

	if config.DefaultConfig.LoadBalancerConnectivity == config.Tunnel && syscall.Geteuid() != 0 {
		// Tunneling on macOS/linux requires elevated privilege
		switch runtime.GOOS {
		case "darwin":
			return fmt.Errorf("please run this again with `sudo` or --enable-lb-port-mapping=false")
		case "linux":
			return fmt.Errorf("please run this again with `sudo` or --enable-lb-tunnel=false")
		}
	}

	kindProvider := cluster.NewProvider(
		option,
		cluster.ProviderWithLogger(logger),
	)
	controller.New(kindProvider).Run(ctx)
	return nil
}

func isWSL2() bool {
	if v, err := os.ReadFile("/proc/version"); err == nil {
		return strings.Contains(string(v), "WSL2")
	}

	return false
}
