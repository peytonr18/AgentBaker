package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Azure/agentbaker/aks-node-controller/helpers"
	"github.com/Azure/agentbaker/aks-node-controller/pkg/provisioning"
	"github.com/urfave/cli/v3"
)

// ReportFlags carries the CLI options shared by the report-ready and report-failure commands.
type ReportFlags struct {
	Endpoint          string
	Agent             string
	TotalRetryTimeout time.Duration

	// Failure-only fields.
	Reason           string
	DocumentationURL string
	SupportingData   [][2]string
}

func (f ReportFlags) toOptions() provisioning.Options {
	return provisioning.Options{
		Endpoint:          f.Endpoint,
		Agent:             f.Agent,
		TotalRetryTimeout: f.TotalRetryTimeout,
		DocumentationURL:  f.DocumentationURL,
		SupportingData:    f.SupportingData,
	}
}

// reportReadyCommand builds the `report-ready` cli command.
func (a *App) reportReadyCommand() *cli.Command {
	return &cli.Command{
		Name:  "report-ready",
		Usage: "Report provisioning success (Ready) to the Azure wireserver",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "endpoint", Usage: "wireserver provisioning-health endpoint", Value: provisioning.DefaultHealthEndpoint},
			&cli.StringFlag{Name: "agent", Usage: "reporting agent identifier", Value: provisioning.AgentName},
			&cli.DurationFlag{Name: "total-retry-timeout", Usage: "total retry budget for delivering the report", Value: provisioning.DefaultTotalRetryTimeout},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return a.runReportReadyCommand(ctx, ReportFlags{
				Endpoint:          cmd.String("endpoint"),
				Agent:             cmd.String("agent"),
				TotalRetryTimeout: cmd.Duration("total-retry-timeout"),
			})
		},
	}
}

// reportFailureCommand builds the `report-failure` cli command.
func (a *App) reportFailureCommand() *cli.Command {
	return &cli.Command{
		Name:  "report-failure",
		Usage: "Report provisioning failure (NotReady) to the Azure wireserver",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "endpoint", Usage: "wireserver provisioning-health endpoint", Value: provisioning.DefaultHealthEndpoint},
			&cli.StringFlag{Name: "agent", Usage: "reporting agent identifier", Value: provisioning.AgentName},
			&cli.DurationFlag{Name: "total-retry-timeout", Usage: "total retry budget for delivering the report", Value: provisioning.DefaultTotalRetryTimeout},
			&cli.StringFlag{Name: "reason", Aliases: []string{"description"}, Usage: "human-readable failure reason"},
			&cli.StringFlag{Name: "documentation-url", Usage: "documentation URL included in the failure report", Value: provisioning.DocumentationURL},
			&cli.StringSliceFlag{Name: "supporting-data", Usage: "additional key=value context (repeatable)"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			supporting, err := parseSupportingData(cmd.StringSlice("supporting-data"))
			if err != nil {
				return err
			}
			return a.runReportFailureCommand(ctx, ReportFlags{
				Endpoint:          cmd.String("endpoint"),
				Agent:             cmd.String("agent"),
				TotalRetryTimeout: cmd.Duration("total-retry-timeout"),
				Reason:            cmd.String("reason"),
				DocumentationURL:  cmd.String("documentation-url"),
				SupportingData:    supporting,
			})
		},
	}
}

// runReportReadyCommand reports provisioning success to the wireserver. It is fail-closed: the
// error propagates so the process exit code is non-zero when the report cannot be delivered.
func (a *App) runReportReadyCommand(ctx context.Context, flags ReportFlags) error {
	slog.Info("aks-node-controller report-ready started")
	startTime := time.Now()
	a.logReportEvent("ReportReady", "Starting", helpers.EventLevelInformational, startTime, startTime)

	err := provisioning.ReportReady(ctx, flags.toOptions())

	endTime := time.Now()
	if err != nil {
		a.logReportEvent("ReportReady", fmt.Sprintf("report-ready failed: %s", err.Error()), helpers.EventLevelError, startTime, endTime)
		slog.Error("aks-node-controller report-ready failed", "error", err)
		return err
	}
	a.logReportEvent("ReportReady", "Completed", helpers.EventLevelInformational, startTime, endTime)
	slog.Info("aks-node-controller report-ready finished successfully")
	return nil
}

// runReportFailureCommand reports provisioning failure to the wireserver. Like report-ready it
// is fail-closed: a delivery failure yields a non-zero exit code. Successfully reporting a
// provisioning failure exits 0 because the report itself was delivered.
func (a *App) runReportFailureCommand(ctx context.Context, flags ReportFlags) error {
	slog.Info("aks-node-controller report-failure started", "reason", flags.Reason)
	startTime := time.Now()
	a.logReportEvent("ReportFailure", "Starting", helpers.EventLevelInformational, startTime, startTime)

	err := provisioning.ReportFailure(ctx, flags.toOptions(), flags.Reason)

	endTime := time.Now()
	if err != nil {
		a.logReportEvent("ReportFailure", fmt.Sprintf("report-failure failed: %s", err.Error()), helpers.EventLevelError, startTime, endTime)
		slog.Error("aks-node-controller report-failure failed", "error", err)
		return err
	}
	a.logReportEvent("ReportFailure", "Completed", helpers.EventLevelInformational, startTime, endTime)
	slog.Info("aks-node-controller report-failure finished successfully")
	return nil
}

// logReportEvent emits a guest agent event, guarding against a nil eventLogger so the report
// commands remain usable in tests that construct an App without one.
func (a *App) logReportEvent(task, message string, level helpers.EventLevel, start, end time.Time) {
	if a.eventLogger == nil {
		return
	}
	a.eventLogger.LogEvent(task, message, level, start, end)
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
