package main

import (
	"sigs.k8s.io/cloud-provider-kind/cmd/app"

	_ "sigs.k8s.io/cloud-provider-kind/pkg/provider" // initialize cloud provider
)

func main() {
	app.Main()
}
