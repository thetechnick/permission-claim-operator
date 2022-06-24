package main

import (
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/pprof"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	permissionapis "github.com/thetechnick/permission-claim-operator/apis"
	"github.com/thetechnick/permission-claim-operator/internal/controllers"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

var (
	scheme       = runtime.NewScheme()
	targetScheme = runtime.NewScheme()
	setupLog     = ctrl.Log.WithName("setup")
)

func init() {
	_ = clientgoscheme.AddToScheme(scheme)
	_ = clientgoscheme.AddToScheme(targetScheme)
	_ = permissionapis.AddToScheme(scheme)
}

type opts struct {
	metricsAddr             string
	pprofAddr               string
	enableLeaderElection    bool
	namespace               string
	probeAddr               string
	targetClusterKubeconfig string
}

func main() {
	var opts opts
	flag.StringVar(&opts.metricsAddr, "metrics-addr", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&opts.pprofAddr, "pprof-addr", "", "The address the pprof web endpoint binds to.")
	flag.StringVar(&opts.namespace, "namespace", os.Getenv("PKO_NAMESPACE"), "xx")
	flag.BoolVar(&opts.enableLeaderElection, "enable-leader-election", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.StringVar(&opts.probeAddr, "health-probe-bind-address", ":8081",
		"The address the probe endpoint binds to.")
	flag.StringVar(&opts.targetClusterKubeconfig, "remote-cluster-kubeconfig", "", "Target cluster kubeconfig.")
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	if err := run(opts); err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}
}

func run(opts opts) error {
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                     scheme,
		MetricsBindAddress:         opts.metricsAddr,
		HealthProbeBindAddress:     opts.probeAddr,
		Port:                       9443,
		LeaderElectionResourceLock: "leases",
		LeaderElection:             opts.enableLeaderElection,
		LeaderElectionID:           "8a4hp84a6s.package-operator-lock",
	})
	if err != nil {
		return fmt.Errorf("creating manager: %w", err)
	}

	if err := mgr.AddHealthzCheck("health", healthz.Ping); err != nil {
		return fmt.Errorf("unable to set up health check: %w", err)
	}
	if err := mgr.AddReadyzCheck("check", healthz.Ping); err != nil {
		return fmt.Errorf("unable to set up ready check: %w", err)
	}

	// -----
	// PPROF
	// -----
	if len(opts.pprofAddr) > 0 {
		mux := http.NewServeMux()
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

		s := &http.Server{Addr: opts.pprofAddr, Handler: mux}
		err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
			errCh := make(chan error)
			defer func() {
				for range errCh {
				} // drain errCh for GC
			}()
			go func() {
				defer close(errCh)
				errCh <- s.ListenAndServe()
			}()

			select {
			case err := <-errCh:
				return err
			case <-ctx.Done():
				s.Close()
				return nil
			}
		}))
		if err != nil {
			return fmt.Errorf("unable to create pprof server: %w", err)
		}
	}

	// TargetCluster Kubeconfig
	targetKubeconfigBytes, err := ioutil.ReadFile(opts.targetClusterKubeconfig)
	if err != nil {
		return fmt.Errorf("reading target cluster kubeconfig: %w", err)
	}
	targetKubeconfig := &clientcmdapi.Config{}
	if err := yaml.Unmarshal(targetKubeconfigBytes, targetKubeconfig); err != nil {
		return fmt.Errorf("parsing target cluster kubeconfig: %w", err)
	}

	// TargetCluster clients
	targetCfg, err := clientcmd.BuildConfigFromFlags("", opts.targetClusterKubeconfig)
	if err != nil {
		return fmt.Errorf("reading target cluster kubeconfig: %w", err)
	}
	targetMapper, err := apiutil.NewDiscoveryRESTMapper(targetCfg)
	if err != nil {
		return fmt.Errorf("creating target cluster rest mapper: %w", err)
	}
	targetClient, err := client.New(targetCfg, client.Options{
		Scheme: targetScheme,
		Mapper: targetMapper,
	})
	if err != nil {
		return fmt.Errorf("creating target cluster client: %w", err)
	}
	targetCache, err := cache.New(targetCfg, cache.Options{
		Scheme: targetScheme,
		Mapper: targetMapper,
	})
	if err != nil {
		return fmt.Errorf("creating target cluster cache: %w", err)
	}
	if err := mgr.Add(targetCache); err != nil {
		return fmt.Errorf("adding target cluster cache to manager: %w", err)
	}
	targetCachedClient, err := client.NewDelegatingClient(client.NewDelegatingClientInput{
		CacheReader: targetCache,
		Client:      targetClient,
	})
	if err != nil {
		return fmt.Errorf("creating cached client for target cluster: %w", err)
	}

	// Package
	if err = (controllers.NewPermissionClaimController(
		ctrl.Log.WithName("controllers").WithName("ClusterPackage"),
		mgr.GetClient(), mgr.GetScheme(), targetKubeconfig, targetCachedClient, targetCache,
	).SetupWithManager(mgr)); err != nil {
		return fmt.Errorf("unable to create controller for ClusterPackage: %w", err)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		return fmt.Errorf("problem running manager: %w", err)
	}
	return nil
}
