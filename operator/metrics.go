package main

import metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

func metricsOptions() metricsserver.Options {
	return metricsserver.Options{BindAddress: ":8080"}
}
