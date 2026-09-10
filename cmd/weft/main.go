// Command weft runs the Weave controller.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/alethic/weft/api/v1alpha1"
	"github.com/alethic/weft/internal/controller"
	"github.com/alethic/weft/internal/eval"
	"github.com/alethic/weft/internal/kube"
	"github.com/alethic/weft/internal/metrics"
	"github.com/alethic/weft/internal/naming"
	"github.com/alethic/weft/internal/version"
	"github.com/alethic/weft/internal/watches"
)

var scheme = runtime.NewScheme()

func init() {
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		panic(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		panic(err)
	}
}

func main() {
	// A tiny subcommand split rather than a CLI framework: there are two modes
	// and one of them exists only for uninstall.
	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "--version" || os.Args[1] == "-version") {
		fmt.Println(version.Get())
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "reap" {
		if err := runReap(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "weft reap: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if err := runManager(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "weft: %v\n", err)
		os.Exit(1)
	}
}

type config struct {
	metricsAddr    string
	probeAddr      string
	leaderElect    bool
	leaderElectID  string
	groups         string
	watchNamespace string

	// pruneThreshold is bound separately because the flag package has no
	// int32 variant.
	pruneThreshold int

	opts    controller.Options
	evalOpt eval.Options
	watch   watches.Options
}

func bindFlags(fs *flag.FlagSet, c *config) {
	d := controller.DefaultOptions()

	fs.StringVar(&c.metricsAddr, "metrics-bind-address", "0", "Address the metrics endpoint binds to; 0 disables it.")
	fs.StringVar(&c.probeAddr, "health-probe-bind-address", ":8081", "Address the health probe endpoint binds to.")
	fs.BoolVar(&c.leaderElect, "leader-elect", false, "Elect a leader so only one replica reconciles.")
	fs.StringVar(&c.leaderElectID, "leader-election-id", naming.Group, "Name of the leader election lease.")
	fs.StringVar(&c.watchNamespace, "namespace", "", "Restrict to a single namespace. Empty watches all of them.")

	fs.StringVar(&c.groups, "impersonate-groups",
		strings.Join(kube.DefaultGroups, ","),
		"Groups to impersonate alongside the ServiceAccount, comma separated. "+
			""+NamespaceHelp+" Set to an empty string to impersonate the username only, "+
			"which is weaker than a real ServiceAccount token and will not match RBAC bound to system:authenticated.")

	fs.IntVar(&c.pruneThreshold, "prune-threshold", int(d.PruneThreshold),
		"Consecutive successful evaluations a resource must be absent from before it is deleted. "+
			"Managed resources drop their status transiently while a provider restarts, and deleting on the first "+
			"sight of that churns real infrastructure.")
	fs.DurationVar(&c.opts.PruneDelay, "prune-delay", d.PruneDelay,
		"How long a resource must have been continuously absent from successful evaluations before it is "+
			"deleted. This is the half of the hysteresis that actually protects anything: reconciles are "+
			"event-driven, so a count of them measures controller activity rather than elapsed time.")
	fs.DurationVar(&c.opts.HoldTimeout, "hold-timeout", d.HoldTimeout,
		"How long a finalizer placed by read(..., hold=True) may block that resource's deletion before it is "+
			"released anyway.")
	fs.DurationVar(&c.opts.TeardownTimeout, "teardown-timeout", d.TeardownTimeout,
		"How long ordered teardown of a deleting Weave may run before the rest is handed to cascading collection.")
	fs.DurationVar(&c.opts.PollInterval, "poll-interval", d.PollInterval,
		"Requeue period for a Weave whose watches could not be established.")
	fs.DurationVar(&c.opts.Backstop, "backstop-interval", d.Backstop,
		"Requeue period in the normal case, bounding how long a missed event goes unnoticed.")
	fs.DurationVar(&c.opts.DegradedRetry, "degraded-retry", d.DegradedRetry,
		"Requeue period while a Weave is Degraded.")

	e := eval.DefaultOptions()
	fs.Uint64Var(&c.evalOpt.MaxSteps, "max-steps", e.MaxSteps, "Execution budget for one call to compose().")
	fs.IntVar(&c.evalOpt.MaxResources, "max-resources", e.MaxResources, "Most resources one evaluation may return.")
	fs.IntVar(&c.evalOpt.MaxValues, "max-values", e.MaxValues, "Most values one evaluation's result may contain.")
	fs.IntVar(&c.evalOpt.MaxReads, "max-reads", e.MaxReads,
		"Most distinct resources one evaluation may read. Every read is also a watch the controller keeps alive.")
	fs.IntVar(&c.evalOpt.MaxSelected, "max-selected", e.MaxSelected, "Most objects one select() may match.")
	fs.IntVar(&c.evalOpt.CacheSize, "program-cache-size", e.CacheSize, "Compiled programs to retain.")

	fs.DurationVar(&c.watch.Resync, "watch-resync", 10*time.Minute, "Informer resync period.")
	fs.DurationVar(&c.watch.Lifetime, "watch-lifetime", 30*time.Minute,
		"How long a watch runs before it is re-established. Authorisation is checked when a watch is opened and "+
			"not continuously, so this bounds how long a revoked grant keeps being honoured.")
}

// NamespaceHelp documents the placeholder available in --impersonate-groups.
const NamespaceHelp = "{namespace} is replaced with the Weave's namespace."

func runManager(args []string) error {
	cfg := &config{}
	fs := flag.NewFlagSet("weft", flag.ExitOnError)
	bindFlags(fs, cfg)
	zapOpts := zap.Options{Development: false}
	zapOpts.BindFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg.opts.PruneThreshold = int32(cfg.pruneThreshold)

	logf.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))
	log := ctrl.Log.WithName("setup")

	mgrOpts := ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: cfg.metricsAddr},
		HealthProbeBindAddress: cfg.probeAddr,
		LeaderElection:         cfg.leaderElect,
		LeaderElectionID:       cfg.leaderElectID + ".lock",
	}
	if cfg.watchNamespace != "" {
		mgrOpts.Cache.DefaultNamespaces = map[string]cache.Config{cfg.watchNamespace: {}}
	}

	restCfg := ctrl.GetConfigOrDie()
	mgr, err := ctrl.NewManager(restCfg, mgrOpts)
	if err != nil {
		return fmt.Errorf("building manager: %w", err)
	}

	factory, err := kube.NewFactory(restCfg, mgr.GetRESTMapper(), kube.FactoryOptions{
		Groups:    parseGroups(cfg.groups),
		UserAgent: naming.Group + "/" + naming.Version,
	})
	if err != nil {
		return fmt.Errorf("building impersonation factory: %w", err)
	}

	cfg.watch.Log = ctrl.Log.WithName("watches")
	registry := watches.NewRegistry(cfg.watch)
	if err := mgr.Add(registry); err != nil {
		return fmt.Errorf("registering watch registry: %w", err)
	}

	reconciler := &controller.WeaveReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Factory:   factory,
		Evaluator: eval.NewStarlark(cfg.evalOpt),
		Watches:   registry,
		// The newer events API would also change the RBAC this controller
		// needs, from events in the core group to events.k8s.io, and getting
		// that wrong makes events silently disappear. Migrating is a change of
		// its own, not a drive-by.
		//nolint:staticcheck // SA1019: deliberate; see above.
		Recorder: mgr.GetEventRecorderFor(naming.Group),
		Opts:     cfg.opts,
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("registering controller: %w", err)
	}

	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("ping", healthz.Ping); err != nil {
		return err
	}

	build := version.Get()
	metrics.BuildInfo.WithLabelValues(build.Version, build.Commit, build.GoVersion).Set(1)

	log.Info("starting",
		"build", build.Version, "commit", build.Commit,
		"group", naming.Group, "apiVersion", naming.Version,
		"impersonateGroups", parseGroups(cfg.groups))
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		return fmt.Errorf("running manager: %w", err)
	}
	return nil
}

// parseGroups splits the comma-separated group flag. An empty string means
// impersonate no groups; the flag default supplies the normal set.
func parseGroups(s string) []string {
	if strings.TrimSpace(s) == "" {
		return []string{}
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
