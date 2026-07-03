// Command harness is the drop-measurement tool for the Istio ambient upgrade
// lab. It has three subcommands:
//
//	echo     - a minimal TCP echo server (the per-node peer the probe dials).
//	probe    - holds a long-lived connection to the same-node echo Pod, opens
//	           continuous new connections, and emits JSON-line ConnEvents.
//	load     - concurrent load generator: sustains N workers holding a mix of
//	           long-lived and short-lived connections to the same-node echo (plus
//	           non-verdict realistic app-a traffic), emitting JSON-line
//	           ConnEvents so the upgrade is observed under pressure.
//	measure  - orchestrates a ztunnel upgrade: fires the trigger, watches the
//	           roll windows, collects probe events, runs the pure analyzer, and
//	           prints/persists the machine-readable Result (schemaVersion
//	           harness/v1). Exits 0 PASS / 1 FAIL / 2 ERROR.
//	report   - renders a Markdown PASS/FAIL report from a measure Result JSON
//	           (plus optional per-client observations). Exits 0 on any successful
//	           render (PASS/FAIL/ERROR are report CONTENT), non-zero only on an
//	           IO/decode/schemaVersion error.
//
// The verdict logic lives in internal/measure (pure, hermetically tested); all
// cluster/net/git/exec IO lives here and in internal/live.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/MateSousa/istio-ambient-upgrade-lab/harness/internal/live"
	"github.com/MateSousa/istio-ambient-upgrade-lab/harness/internal/load"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	sub := os.Args[1]
	args := os.Args[2:]

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	switch sub {
	case "echo":
		runEcho(args)
	case "probe":
		runProbe(ctx, args)
	case "load":
		runLoad(ctx, args)
	case "measure":
		runMeasure(ctx, args)
	case "next-version":
		runNextVersion(args)
	case "report":
		runReport(args)
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", sub)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `harness - Istio ambient upgrade drop-measurement

usage:
  harness echo                 run the TCP echo server (ECHO_LISTEN, default :9000)
  harness probe                run the per-node probe (ECHO_ADDR, NODE_NAME, ...)
  harness load [flags]         run the concurrent load generator (ECHO_ADDR, ...)
  harness measure [flags]      orchestrate a ztunnel upgrade and measure drops
  harness next-version [flags] print the next fresh umbrella chart version (single authority)
  harness report [flags]       render a Markdown PASS/FAIL report from a Result JSON
`)
}

func runEcho(args []string) {
	fs := flag.NewFlagSet("echo", flag.ExitOnError)
	_ = fs.Parse(args)
	if err := live.RunEcho(live.EchoConfigFromEnv()); err != nil {
		fmt.Fprintf(os.Stderr, "echo: %v\n", err)
		os.Exit(1)
	}
}

func runProbe(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("probe", flag.ExitOnError)
	_ = fs.Parse(args)
	cfg, err := live.ProbeConfigFromEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "probe: %v\n", err)
		os.Exit(2)
	}
	if err := live.RunProbe(ctx, cfg); err != nil && ctx.Err() == nil {
		fmt.Fprintf(os.Stderr, "probe: %v\n", err)
		os.Exit(1)
	}
}

func runLoad(ctx context.Context, args []string) {
	cfg, err := load.ConfigFromEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "load: %v\n", err)
		os.Exit(2)
	}
	fs := flag.NewFlagSet("load", flag.ExitOnError)
	fs.IntVar(&cfg.Concurrency, "concurrency", cfg.Concurrency, "total concurrent workers")
	fs.Float64Var(&cfg.LongFraction, "long-fraction", cfg.LongFraction, "fraction of workers holding long-lived connections")
	fs.DurationVar(&cfg.Hold, "hold", cfg.Hold, "long-lived hold duration (must outlive the ztunnel drain)")
	fs.DurationVar(&cfg.KeepAlive, "keepalive", cfg.KeepAlive, "held-connection trickle/keepalive period")
	fs.DurationVar(&cfg.ShortInterval, "short-interval", cfg.ShortInterval, "short-lived churn period")
	fs.DurationVar(&cfg.Ramp, "ramp", cfg.Ramp, "staggered worker start window")
	fs.IntVar(&cfg.MaxRPS, "max-rps", cfg.MaxRPS, "aggregate app-a request cap")
	fs.DurationVar(&cfg.Duration, "duration", cfg.Duration, "run duration (0 = until cancel)")
	fs.StringVar(&cfg.EchoAddr, "echo-addr", cfg.EchoAddr, "same-node echo IP:port")
	fs.StringVar(&cfg.AppAURL, "appa-url", cfg.AppAURL, "app-a query URL (empty disables app-a traffic)")
	fs.StringVar(&cfg.ReadinessAddr, "readiness-addr", cfg.ReadinessAddr, "readiness gate listen addr")
	_ = fs.Parse(args)

	// The --hold flag overwrites cfg.Hold after ConfigFromEnv validated it, so
	// re-gate through the same floor here or a sub-drain --hold slips through.
	if err := load.ValidateHold(cfg.Hold); err != nil {
		fmt.Fprintf(os.Stderr, "load: %v\n", err)
		os.Exit(2)
	}

	if err := load.RunLoad(ctx, cfg); err != nil && ctx.Err() == nil {
		fmt.Fprintf(os.Stderr, "load: %v\n", err)
		os.Exit(1)
	}
}

func runMeasure(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("measure", flag.ExitOnError)
	cfg := live.DefaultMeasureConfig()
	fs.StringVar(&cfg.TriggerKind, "trigger", cfg.TriggerKind, "trigger kind: git-bump | rollout-restart")
	fs.StringVar(&cfg.RepoRoot, "repo-root", ".", "path to the lab repo working copy (git-bump)")
	fs.StringVar(&cfg.Hop, "hop", cfg.Hop, "git-bump hop: patch (ztunnel dep only) | minor (all four deps + appVersion)")
	fs.StringVar(&cfg.ZtunnelFrom, "ztunnel-from", "1.29.2", "current ztunnel version (git-bump patch hop)")
	fs.StringVar(&cfg.ZtunnelTo, "ztunnel-to", "1.29.5", "target ztunnel version (git-bump patch hop)")
	fs.StringVar(&cfg.VersionFrom, "version-from", "1.29.2", "current istio version for all deps + appVersion (git-bump minor hop)")
	fs.StringVar(&cfg.VersionTo, "version-to", "1.30.0", "target istio version (git-bump minor hop)")
	fs.StringVar(&cfg.ChartVersionTo, "chart-version-to", "1.0.1", "umbrella chart version to publish (git-bump)")
	fs.StringVar(&cfg.OutPath, "out", "-", "Result JSON destination ('-' => stdout)")
	fs.StringVar(&cfg.OutClientsPath, "out-clients", "", "per-client observations JSON destination ('' => producer disabled)")
	fs.StringVar(&cfg.ProbeNamespace, "probe-namespace", cfg.ProbeNamespace, "namespace holding the probe Pods")
	fs.DurationVar(&cfg.Deadline, "deadline", cfg.Deadline, "observation window")
	fs.IntVar(&cfg.RecoveryBound, "recovery-bound", cfg.RecoveryBound, "recovery bound seconds")
	fs.Float64Var(&cfg.JitterEps, "jitter-eps", cfg.JitterEps, "jitter tolerance seconds")
	ack := fs.Bool("i-know-this-is-not-ac", false, "acknowledge the rollout-restart escape hatch is NOT the AC-satisfying path")
	_ = fs.Parse(args)
	cfg.Acknowledged = *ack

	res, code, err := live.RunMeasure(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "measure: %v\n", err)
	}
	_ = res
	os.Exit(code)
}

// runNextVersion prints the next fresh umbrella chart version for a hop. It is
// the ONE authority the scenario scripts call so a run never invents a version
// string in shell (which would risk drifting from the Go bump logic).
func runNextVersion(args []string) {
	fs := flag.NewFlagSet("next-version", flag.ExitOnError)
	current := fs.String("current", "", "current umbrella chart version (required, e.g. 1.0.0)")
	hop := fs.String("hop", "patch", "hop: patch | minor")
	runTag := fs.String("run-tag", "", "prerelease run tag (required; appended as -dev<runTag>)")
	_ = fs.Parse(args)
	v, err := live.NextChartVersion(*current, *hop, *runTag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "next-version: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(v)
}

func runReport(args []string) {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	in := fs.String("in", "results.json", "Result JSON input")
	clients := fs.String("clients", "", "per-client observations JSON (optional)")
	out := fs.String("out", "report.md", "Markdown destination ('-' => stdout)")
	_ = fs.Parse(args)

	code, err := live.RunReport(live.ReportConfig{
		InPath:      *in,
		ClientsPath: *clients,
		OutPath:     *out,
		GeneratedAt: time.Now().UTC(),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "report: %v\n", err)
	}
	os.Exit(code)
}
