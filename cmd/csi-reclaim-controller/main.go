// Command csi-reclaim-controller is an optional CSI sidecar that deletes the
// backing storage of PersistentVolumes using "reclaimPolicy: Retain" once the
// PV object is deleted.
//
// By default the Retain policy intentionally leaves backing volumes in place
// when a PV is removed. Deploying this sidecar alongside a CSI driver opts every
// Retain PV of that driver into automatic backend cleanup: the sidecar places a
// finalizer on such PVs and, when one is deleted, calls DeleteVolume() before
// releasing the finalizer.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	csiconfig "github.com/kubernetes-csi/csi-lib-utils/config"
	"github.com/kubernetes-csi/csi-lib-utils/connection"
	"github.com/kubernetes-csi/csi-lib-utils/leaderelection"
	"github.com/kubernetes-csi/csi-lib-utils/metrics"
	"github.com/kubernetes-csi/csi-lib-utils/rpc"
	"github.com/kubernetes-csi/csi-lib-utils/standardflags"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"

	"github.com/piraeusdatastore/csi-reclaim-controller/pkg/controller"
	"github.com/piraeusdatastore/csi-reclaim-controller/pkg/metadata"
	"github.com/piraeusdatastore/csi-reclaim-controller/pkg/reclaim"
)

// Component-specific flags. Flags common to CSI sidecars (--csi-address,
// --kubeconfig, --leader-election*, --http-endpoint, --metrics-*, --version,
// --kube-api-*) are provided by csi-lib-utils/standardflags.
// Deploying this controller opts every Retain PV of the driver into reclaiming;
// individual PVs can be excluded with the skip annotation. The skip annotation
// and finalizer default to keys based on the CSI driver name so they are unique
// per driver.
var (
	finalizer      = flag.String("finalizer", "", "Finalizer placed on managed PVs. If empty, defaults to \"csi-reclaim-controller/<sanitized-driver-name>\".")
	skipAnnotation = flag.String("skip-annotation", "", "PV annotation that, when set to a true value, excludes the PV from reclaiming (used by admins or by controllers that recreate PVs in place). If empty, defaults to \"<driver-name>/skip-reclaim\".")
	resyncPeriod   = flag.Duration("resync-period", 10*time.Minute, "Resync interval of the PersistentVolume informer.")
	workerCount    = flag.Int("worker-threads", 5, "Number of concurrent reconcile workers.")
	csiTimeout     = flag.Duration("timeout", 60*time.Second, "Timeout for individual CSI DeleteVolume and Probe calls.")
)

func main() {
	klog.InitFlags(nil)
	standardflags.RegisterCommonFlags(flag.CommandLine)
	standardflags.AddAutomaxprocs(klog.Infof)
	flag.Parse()

	if err := run(); err != nil {
		klog.ErrorS(err, "Fatal error")
		klog.FlushAndExit(klog.ExitFlushTimeout, 1)
	}
}

func run() error {
	if standardflags.Configuration.ShowVersion {
		fmt.Printf("csi-reclaim-controller %s\n", metadata.Version)
		return nil
	}
	klog.InfoS("csi-reclaim-controller starting", "version", metadata.Version)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	config, err := csiconfig.BuildConfig(standardflags.Configuration.KubeConfig, standardflags.Configuration)
	if err != nil {
		return fmt.Errorf("build kube config: %w", err)
	}

	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("build kube client: %w", err)
	}

	// Connect to the CSI driver and learn its identity and capabilities.
	metricsManager := metrics.NewCSIMetricsManagerForSidecar("")
	csiAddress := standardflags.Configuration.CSIAddress
	conn, err := connection.Connect(ctx, csiAddress, metricsManager,
		connection.OnConnectionLoss(connection.ExitOnConnectionLoss()))
	if err != nil {
		return fmt.Errorf("connect to CSI driver at %s: %w", csiAddress, err)
	}

	klog.InfoS("Probing CSI driver", "address", csiAddress)
	if err := rpc.ProbeForever(ctx, conn, *csiTimeout); err != nil {
		return fmt.Errorf("probe CSI driver: %w", err)
	}

	resolvedDriver, err := rpc.GetDriverName(ctx, conn)
	if err != nil {
		return fmt.Errorf("get CSI driver name: %w", err)
	}
	metricsManager.SetDriverName(resolvedDriver)

	caps, err := rpc.GetControllerCapabilities(ctx, conn)
	if err != nil {
		return fmt.Errorf("get controller capabilities: %w", err)
	}
	if !caps[csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME] {
		return fmt.Errorf("driver %q does not advertise the CREATE_DELETE_VOLUME capability; cannot reclaim volumes", resolvedDriver)
	}

	resolvedFinalizer := *finalizer
	if resolvedFinalizer == "" {
		resolvedFinalizer = reclaim.Finalizer(resolvedDriver)
	}

	resolvedSkipAnnotation := *skipAnnotation
	if resolvedSkipAnnotation == "" {
		resolvedSkipAnnotation = reclaim.SkipAnnotation(resolvedDriver)
	}

	recorder := eventRecorder(kubeClient)
	deleter := controller.NewCSIVolumeDeleter(conn, *csiTimeout)
	informerFactory := informers.NewSharedInformerFactory(kubeClient, *resyncPeriod)
	ctrl, err := controller.New(kubeClient, informerFactory, deleter, resolvedDriver, resolvedFinalizer, resolvedSkipAnnotation, recorder)
	if err != nil {
		return fmt.Errorf("create controller: %w", err)
	}

	// Diagnostics: metrics plus (via RunWithLeaderElection) the leader-election
	// health check are served on the shared mux.
	mux := http.NewServeMux()
	metricsManager.RegisterToServer(mux, standardflags.Configuration.MetricsPath)
	if httpAddr := diagnosticsAddress(); httpAddr != "" {
		serveHTTP(ctx, httpAddr, mux)
	}

	runController := func(ctx context.Context) {
		informerFactory.Start(ctx.Done())
		if err := ctrl.Run(ctx, *workerCount); err != nil {
			klog.ErrorS(err, "Reclaim controller exited with error")
			klog.FlushAndExit(klog.ExitFlushTimeout, 1)
		}
	}

	klog.InfoS("Starting csi-reclaim-controller",
		"driver", resolvedDriver, "finalizer", resolvedFinalizer,
		"skipAnnotation", resolvedSkipAnnotation,
		"leaderElection", standardflags.Configuration.LeaderElection)

	// A lock name distinct from the driver name avoids colliding with the
	// driver's own leases (e.g. external-provisioner's).
	lockName := "csi-reclaim-controller-" + resolvedDriver
	leaderelection.RunWithLeaderElection(ctx, config, standardflags.Configuration,
		runController, lockName, mux, true)
	return nil
}

func eventRecorder(kubeClient kubernetes.Interface) record.EventRecorder {
	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{
		Interface: kubeClient.CoreV1().Events(""),
	})
	return broadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "csi-reclaim-controller"})
}

// diagnosticsAddress returns the address for the diagnostics HTTP server,
// preferring --http-endpoint over the deprecated --metrics-address.
func diagnosticsAddress() string {
	if standardflags.Configuration.HttpEndpoint != "" {
		return standardflags.Configuration.HttpEndpoint
	}
	return standardflags.Configuration.MetricsAddress
}

func serveHTTP(ctx context.Context, addr string, mux *http.ServeMux) {
	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	go func() {
		klog.InfoS("Serving diagnostics", "address", addr, "metricsPath", standardflags.Configuration.MetricsPath)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			klog.ErrorS(err, "Diagnostics HTTP server failed")
		}
	}()
}
