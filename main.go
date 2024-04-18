package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"k8s.io/component-base/logs"

	"sigs.k8s.io/cloud-provider-kind/pkg/controller"

	kindcmd "sigs.k8s.io/kind/pkg/cmd"
)

var (
	flagV int
)

func init() {
	flag.IntVar(&flagV, "v", 2, "Verbosity level")

	flag.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: cloud-provider-kind [options]\n\n")
		flag.PrintDefaults()
	}
}

func main() {
	// Parse command line flags and arguments
	flag.Parse()

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
			log.Printf("Exiting: received signal")
			cancel()
		case <-ctx.Done():
			// cleanup
		}
	}()

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
	controller.New(logger).Run(ctx)
}
