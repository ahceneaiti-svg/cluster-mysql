package main

import (
	"flag"
	"os"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	v1 "github.com/aia/mysql-operator/api/v1alpha1"
	"github.com/aia/mysql-operator/internal/controller"
)

func main() {
	var probeAddr string
	var leader bool
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "adresse des sondes healthz/readyz")
	flag.BoolVar(&leader, "leader-elect", false, "active l'élection de leader (plusieurs réplicas)")
	zopts := zap.Options{}
	zopts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zopts)))

	scheme := runtime.NewScheme()
	utilruntime.Must(corev1.AddToScheme(scheme))
	utilruntime.Must(batchv1.AddToScheme(scheme))
	utilruntime.Must(v1.AddToScheme(scheme))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leader,
		LeaderElectionID:       "mysql-operator.mysql.aia.local",
		Metrics:                metricsOptions(),
	})
	if err != nil {
		fatal(err)
	}
	if err := (&controller.BackupReconciler{Client: mgr.GetClient(), Scheme: scheme}).SetupWithManager(mgr); err != nil {
		fatal(err)
	}
	if err := (&controller.RestoreReconciler{Client: mgr.GetClient(), Scheme: scheme}).SetupWithManager(mgr); err != nil {
		fatal(err)
	}
	if err := (&controller.ScheduleReconciler{Client: mgr.GetClient(), Scheme: scheme}).SetupWithManager(mgr); err != nil {
		fatal(err)
	}
	_ = mgr.AddHealthzCheck("healthz", healthz.Ping)
	_ = mgr.AddReadyzCheck("readyz", healthz.Ping)

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	ctrl.Log.Error(err, "arrêt de l'opérateur")
	os.Exit(1)
}
