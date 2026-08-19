// Command provisioning-report reports AKS node provisioning health (Ready/NotReady) to the
// Azure wireserver and writes the encoded report to the Hyper-V KVP pool. It is a thin CLI
// wrapper over the provisioning package and ships on the VHD at
// /opt/azure/containers/provisioning-report for boot-time and manual use.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/Azure/agentbaker/aks-node-controller/pkg/provisioning"
	"github.com/urfave/cli/v3"
)

// Version is set at build time via -ldflags "-X main.Version=<version>".
var Version = "dev" //nolint:gochecknoglobals // set via ldflags at build time

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	os.Exit(errToExitCode(run(context.Background(), os.Args)))
}

func run(ctx context.Context, args []string) error {
	cmd := &cli.Command{
		Name:    "provisioning-report",
		Usage:   "Report AKS node provisioning health to the Azure wireserver",
		Version: Version,
		Commands: []*cli.Command{
			reportReadyCommand(),
			reportFailureCommand(),
		},
	}
	return cmd.Run(ctx, args)
}

// errToExitCode maps a delivery error to a non-zero exit. Successfully reporting a failure
// returns nil (the report was delivered) and therefore exits 0.
func errToExitCode(err error) int {
	if err == nil {
		return 0
	}
	slog.Error("provisioning-report failed", "error", err)
	return 1
}

// reportFlags carries the CLI options shared by report-ready and report-failure.
type reportFlags struct {
	Endpoint          string
	Agent             string
	TotalRetryTimeout time.Duration
	DisableKVP        bool
	KVPPath           string
	Reason            string
	DocumentationURL  string
	SupportingData    [][2]string
}

func (f reportFlags) toOptions() provisioning.Options {
	return provisioning.Options{
		Endpoint:          f.Endpoint,
		Agent:             f.Agent,
		TotalRetryTimeout: f.TotalRetryTimeout,
		DisableKVP:        f.DisableKVP,
		KVPPath:           f.KVPPath,
		DocumentationURL:  f.DocumentationURL,
		SupportingData:    f.SupportingData,
	}
}

func commonReportFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "endpoint", Usage: "wireserver provisioning-health endpoint", Value: provisioning.DefaultHealthEndpoint},
		&cli.StringFlag{Name: "agent", Usage: "reporting agent identifier", Value: provisioning.AgentName},
		&cli.DurationFlag{Name: "total-retry-timeout", Usage: "total retry budget for delivering the report", Value: provisioning.DefaultTotalRetryTimeout},
		&cli.BoolFlag{Name: "disable-kvp", Usage: "do not write the report and dmesg to the Hyper-V KVP pool"},
		&cli.StringFlag{Name: "kvp-path", Usage: "override the Hyper-V KVP pool file path"},
	}
}

func readCommonReportFlags(cmd *cli.Command) reportFlags {
	return reportFlags{
		Endpoint:          cmd.String("endpoint"),
		Agent:             cmd.String("agent"),
		TotalRetryTimeout: cmd.Duration("total-retry-timeout"),
		DisableKVP:        cmd.Bool("disable-kvp"),
		KVPPath:           cmd.String("kvp-path"),
	}
}

func reportReadyCommand() *cli.Command {
	return &cli.Command{
		Name:  "report-ready",
		Usage: "Report provisioning success (Ready) to the Azure wireserver",
		Flags: commonReportFlags(),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			flags := readCommonReportFlags(cmd)
			slog.Info("report-ready started", "endpoint", flags.Endpoint)
			if err := provisioning.ReportReady(ctx, flags.toOptions()); err != nil {
				return fmt.Errorf("report-ready: %w", err)
			}
			slog.Info("report-ready finished successfully")
			return nil
		},
	}
}

func reportFailureCommand() *cli.Command {
	return &cli.Command{
		Name:  "report-failure",
		Usage: "Report provisioning failure (NotReady) to the Azure wireserver",
		Flags: append(commonReportFlags(),
			&cli.StringFlag{Name: "reason", Aliases: []string{"description"}, Usage: "human-readable failure reason", Required: true},
			&cli.StringFlag{Name: "documentation-url", Usage: "documentation URL included in the failure report", Value: provisioning.DocumentationURL},
			&cli.StringSliceFlag{Name: "supporting-data", Usage: "additional key=value context (repeatable)"},
		),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			supporting, err := parseSupportingData(cmd.StringSlice("supporting-data"))
			if err != nil {
				return err
			}
			flags := readCommonReportFlags(cmd)
			flags.Reason = cmd.String("reason")
			flags.DocumentationURL = cmd.String("documentation-url")
			flags.SupportingData = supporting
			slog.Info("report-failure started", "endpoint", flags.Endpoint, "reason", flags.Reason)
			if err := provisioning.ReportFailure(ctx, flags.toOptions(), flags.Reason); err != nil {
				return fmt.Errorf("report-failure: %w", err)
			}
			slog.Info("report-failure finished successfully")
			return nil
		},
	}
}

// parseSupportingData parses repeatable "key=value" CLI values into ordered pairs.
func parseSupportingData(pairs []string) ([][2]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make([][2]string, 0, len(pairs))
	for _, p := range pairs {
		key, value, ok := strings.Cut(p, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("invalid supporting-data %q, expected key=value", p)
		}
		out = append(out, [2]string{key, value})
	}
	return out, nil
}
