// Package previewcli implements the previewctl command-line client.
package previewcli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"
)

const defaultAPIURL = "http://127.0.0.1:8081"

// BuildInfo is populated by linker flags in release builds.
type BuildInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"built_at"`
}

// Streams makes command execution testable without replacing process globals.
type Streams struct {
	In  io.Reader
	Out io.Writer
	Err io.Writer
}

type app struct {
	streams Streams
	build   BuildInfo
}

// Run executes previewctl and returns a process exit code.
func Run(ctx context.Context, args []string, streams Streams, build BuildInfo) int {
	if streams.In == nil {
		streams.In = os.Stdin
	}
	if streams.Out == nil {
		streams.Out = os.Stdout
	}
	if streams.Err == nil {
		streams.Err = os.Stderr
	}
	if build.Version == "" {
		build.Version = "dev"
	}
	application := &app{streams: streams, build: build}
	if err := application.run(ctx, args); err != nil {
		var help *helpRequest
		if errors.As(err, &help) {
			fmt.Fprint(streams.Out, help.usage)
			return 0
		}
		var usageError *commandUsageError
		if errors.As(err, &usageError) {
			fmt.Fprintln(streams.Err, "Error:", usageError.Error())
			if usageError.usage != "" {
				fmt.Fprintln(streams.Err)
				fmt.Fprint(streams.Err, usageError.usage)
			}
			return 2
		}
		fmt.Fprintln(streams.Err, "Error:", err)
		return 1
	}
	return 0
}

type globalOptions struct {
	apiURL  string
	token   string
	timeout time.Duration
}

func (a *app) run(ctx context.Context, args []string) error {
	global, commandArgs, err := a.parseGlobal(args)
	if err != nil {
		return err
	}
	if len(commandArgs) == 0 {
		fmt.Fprint(a.streams.Out, rootUsage)
		return nil
	}
	command := commandArgs[0]
	commandArgs = commandArgs[1:]
	if command == "help" || command == "--help" || command == "-h" {
		if len(commandArgs) == 0 {
			fmt.Fprint(a.streams.Out, rootUsage)
			return nil
		}
		return a.printCommandHelp(commandArgs[0])
	}
	if command == "version" {
		return a.runVersion(ctx, commandArgs)
	}
	if command == "update" {
		return a.runUpdate(ctx, commandArgs)
	}

	client, err := NewClient(global.apiURL, global.token, a.userAgent(), global.timeout)
	if err != nil {
		return err
	}
	switch command {
	case "deploy":
		return a.runDeploy(ctx, client, commandArgs)
	case "list", "ls":
		return a.runList(ctx, client, commandArgs)
	case "get":
		return a.runGet(ctx, client, commandArgs)
	case "start":
		return a.runLifecycle(ctx, client, commandArgs, "start")
	case "stop":
		return a.runLifecycle(ctx, client, commandArgs, "stop")
	case "delete", "rm":
		return a.runDelete(ctx, client, commandArgs)
	case "logs":
		return a.runLogs(ctx, client, commandArgs)
	case "health":
		return a.runHealth(ctx, client, commandArgs)
	default:
		return usageError(fmt.Sprintf("unknown command %q", command), rootUsage)
	}
}

func (a *app) parseGlobal(args []string) (globalOptions, []string, error) {
	timeout, err := durationFromEnvironment("PREVIEWCTL_TIMEOUT", 15*time.Minute)
	if err != nil {
		return globalOptions{}, nil, usageError(err.Error(), rootUsage)
	}
	options := globalOptions{
		apiURL:  firstNonempty(os.Getenv("PREVIEWCTL_API_URL"), defaultAPIURL),
		token:   os.Getenv("PREVIEWCTL_TOKEN"),
		timeout: timeout,
	}
	flags := flag.NewFlagSet("previewctl", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&options.apiURL, "api-url", options.apiURL, "orchestrator API base URL")
	flags.StringVar(&options.token, "token", options.token, "API bearer token")
	flags.DurationVar(&options.timeout, "timeout", options.timeout, "request timeout")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return globalOptions{}, nil, &helpRequest{usage: rootUsage}
		}
		return globalOptions{}, nil, usageError(err.Error(), rootUsage)
	}
	return options, flags.Args(), nil
}

func (a *app) runDeploy(ctx context.Context, client *Client, args []string) error {
	const usage = `Usage: previewctl [global flags] deploy [--manifest FILE] [--output text|json] SOURCE

SOURCE may be an existing deployment ZIP, an executable, or a directory.
Executables are packaged as root-level app. Directories are packaged into one
ZIP after .dockerignore and .git exclusions; Dockerfile mode requires a root
Dockerfile, while an explicit runtime manifest does not. An optional
preview.json manifest may be supplied with either generated ZIP form.
`
	flags := newCommandFlags("deploy")
	manifest := flags.String("manifest", "", "preview.json to package with an executable or directory")
	output := flags.String("output", "text", "output format: text or json")
	if err := parseCommandFlags(flags, args, usage); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return usageError("deploy requires exactly one SOURCE", usage)
	}
	if err := validateOutput(*output, "text", "json"); err != nil {
		return usageError(err.Error(), usage)
	}
	archive, cleanup, err := prepareArchive(flags.Arg(0), *manifest)
	if err != nil {
		return err
	}
	defer cleanup()
	deployment, err := client.Deploy(ctx, archive)
	if err != nil {
		return err
	}
	return a.printDeployment(deployment, *output)
}

func (a *app) runList(ctx context.Context, client *Client, args []string) error {
	const usage = "Usage: previewctl [global flags] list [--output table|json]\n"
	flags := newCommandFlags("list")
	output := flags.String("output", "table", "output format: table or json")
	if err := parseCommandFlags(flags, args, usage); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return usageError("list does not accept positional arguments", usage)
	}
	if err := validateOutput(*output, "table", "json"); err != nil {
		return usageError(err.Error(), usage)
	}
	deployments, err := client.List(ctx)
	if err != nil {
		return err
	}
	if *output == "json" {
		return writeJSON(a.streams.Out, map[string]any{"deployments": deployments, "count": len(deployments)})
	}
	return writeDeploymentTable(a.streams.Out, deployments)
}

func (a *app) runGet(ctx context.Context, client *Client, args []string) error {
	const usage = "Usage: previewctl [global flags] get [--output text|json] ID\n"
	flags := newCommandFlags("get")
	output := flags.String("output", "text", "output format: text or json")
	if err := parseCommandFlags(flags, args, usage); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return usageError("get requires exactly one deployment ID", usage)
	}
	if err := validateOutput(*output, "text", "json"); err != nil {
		return usageError(err.Error(), usage)
	}
	deployment, err := client.Get(ctx, flags.Arg(0))
	if err != nil {
		return err
	}
	return a.printDeployment(deployment, *output)
}

func (a *app) runLifecycle(ctx context.Context, client *Client, args []string, operation string) error {
	usage := fmt.Sprintf("Usage: previewctl [global flags] %s [--output text|json] ID\n", operation)
	flags := newCommandFlags(operation)
	output := flags.String("output", "text", "output format: text or json")
	if err := parseCommandFlags(flags, args, usage); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return usageError(operation+" requires exactly one deployment ID", usage)
	}
	if err := validateOutput(*output, "text", "json"); err != nil {
		return usageError(err.Error(), usage)
	}
	var deployment Deployment
	var err error
	if operation == "start" {
		deployment, err = client.Start(ctx, flags.Arg(0))
	} else {
		deployment, err = client.Stop(ctx, flags.Arg(0))
	}
	if err != nil {
		return err
	}
	return a.printDeployment(deployment, *output)
}

func (a *app) runDelete(ctx context.Context, client *Client, args []string) error {
	const usage = "Usage: previewctl [global flags] delete [--output text|json] ID\n"
	flags := newCommandFlags("delete")
	output := flags.String("output", "text", "output format: text or json")
	if err := parseCommandFlags(flags, args, usage); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return usageError("delete requires exactly one deployment ID", usage)
	}
	if err := validateOutput(*output, "text", "json"); err != nil {
		return usageError(err.Error(), usage)
	}
	id := flags.Arg(0)
	if err := client.Delete(ctx, id); err != nil {
		return err
	}
	if *output == "json" {
		return writeJSON(a.streams.Out, map[string]any{"deleted": true, "id": id})
	}
	fmt.Fprintf(a.streams.Out, "Deleted deployment %s\n", id)
	return nil
}

func (a *app) runLogs(ctx context.Context, client *Client, args []string) error {
	const usage = "Usage: previewctl [global flags] logs [--tail 1..5000] ID\n"
	flags := newCommandFlags("logs")
	tail := flags.Int("tail", 200, "number of recent lines")
	if err := parseCommandFlags(flags, args, usage); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return usageError("logs requires exactly one deployment ID", usage)
	}
	if *tail < 1 || *tail > 5000 {
		return usageError("--tail must be between 1 and 5000", usage)
	}
	logs, truncated, err := client.Logs(ctx, flags.Arg(0), *tail)
	if err != nil {
		return err
	}
	if _, err := a.streams.Out.Write(logs); err != nil {
		return err
	}
	if truncated {
		fmt.Fprintln(a.streams.Err, "Warning: logs were truncated by the orchestrator")
	}
	return nil
}

func (a *app) runHealth(ctx context.Context, client *Client, args []string) error {
	const usage = "Usage: previewctl [global flags] health [--output text|json]\n"
	flags := newCommandFlags("health")
	output := flags.String("output", "text", "output format: text or json")
	if err := parseCommandFlags(flags, args, usage); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return usageError("health does not accept positional arguments", usage)
	}
	if err := validateOutput(*output, "text", "json"); err != nil {
		return usageError(err.Error(), usage)
	}
	health, err := client.Health(ctx)
	if err != nil {
		return err
	}
	if *output == "json" {
		return writeJSON(a.streams.Out, health)
	}
	fmt.Fprintln(a.streams.Out, health["status"])
	return nil
}

func (a *app) runVersion(ctx context.Context, args []string) error {
	const usage = "Usage: previewctl version [--check] [--output text|json]\n"
	flags := newCommandFlags("version")
	check := flags.Bool("check", false, "check GitHub for the latest release")
	output := flags.String("output", "text", "output format: text or json")
	if err := parseCommandFlags(flags, args, usage); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return usageError("version does not accept positional arguments", usage)
	}
	if err := validateOutput(*output, "text", "json"); err != nil {
		return usageError(err.Error(), usage)
	}
	if !*check {
		if *output == "json" {
			return writeJSON(a.streams.Out, a.build)
		}
		fmt.Fprintf(a.streams.Out, "previewctl %s (commit %s, built %s)\n", displayBuildValue(a.build.Version), displayBuildValue(a.build.Commit), displayBuildValue(a.build.Date))
		return nil
	}
	updater, err := newUpdater(a.build.Version, a.userAgent())
	if err != nil {
		return err
	}
	status, _, err := updater.check(ctx)
	if err != nil {
		return err
	}
	return a.printUpdateCheck(status, *output)
}

func (a *app) runUpdate(ctx context.Context, args []string) error {
	const usage = "Usage: previewctl update [--check] [--force] [--output text|json]\n"
	flags := newCommandFlags("update")
	checkOnly := flags.Bool("check", false, "only check whether an update is available")
	force := flags.Bool("force", false, "reinstall even when already current")
	output := flags.String("output", "text", "output format: text or json")
	if err := parseCommandFlags(flags, args, usage); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return usageError("update does not accept positional arguments", usage)
	}
	if err := validateOutput(*output, "text", "json"); err != nil {
		return usageError(err.Error(), usage)
	}
	updater, err := newUpdater(a.build.Version, a.userAgent())
	if err != nil {
		return err
	}
	status, release, err := updater.check(ctx)
	if err != nil {
		return err
	}
	if *checkOnly {
		return a.printUpdateCheck(status, *output)
	}
	if !status.UpdateAvailable && !*force {
		if *output == "json" {
			return writeJSON(a.streams.Out, map[string]any{"updated": false, "current": status.Current, "latest": status.Latest})
		}
		fmt.Fprintf(a.streams.Out, "previewctl %s is already current\n", status.Current)
		return nil
	}
	if err := updater.install(ctx, release); err != nil {
		return err
	}
	if *output == "json" {
		return writeJSON(a.streams.Out, map[string]any{"updated": true, "previous": status.Current, "current": status.Latest})
	}
	fmt.Fprintf(a.streams.Out, "Updated previewctl from %s to %s\n", status.Current, status.Latest)
	return nil
}

func (a *app) printUpdateCheck(status updateCheck, output string) error {
	if output == "json" {
		return writeJSON(a.streams.Out, status)
	}
	if status.UpdateAvailable {
		fmt.Fprintf(a.streams.Out, "Update available: %s -> %s\nRun `previewctl update` to install it.\n", status.Current, status.Latest)
	} else {
		fmt.Fprintf(a.streams.Out, "previewctl %s is current\n", status.Current)
	}
	return nil
}

func (a *app) printDeployment(deployment Deployment, output string) error {
	if output == "json" {
		return writeJSON(a.streams.Out, deployment)
	}
	writer := tabwriter.NewWriter(a.streams.Out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(writer, "ID\t%s\n", deployment.ID)
	if deployment.Name != "" {
		fmt.Fprintf(writer, "NAME\t%s\n", deployment.Name)
	}
	fmt.Fprintf(writer, "STATUS\t%s\nURL\t%s\n", deployment.Status, deployment.URL)
	return writer.Flush()
}

func writeDeploymentTable(destination io.Writer, deployments []Deployment) error {
	writer := tabwriter.NewWriter(destination, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(writer, "ID\tNAME\tSTATUS\tCREATED\tURL"); err != nil {
		return err
	}
	for _, deployment := range deployments {
		created := "-"
		if !deployment.CreatedAt.IsZero() {
			created = deployment.CreatedAt.Local().Format(time.RFC3339)
		}
		if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", deployment.ID, emptyAsDash(deployment.Name), deployment.Status, created, deployment.URL); err != nil {
			return err
		}
	}
	return writer.Flush()
}

func writeJSON(destination io.Writer, value any) error {
	encoder := json.NewEncoder(destination)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

func (a *app) userAgent() string {
	return "previewctl/" + displayBuildValue(a.build.Version)
}

func (a *app) printCommandHelp(command string) error {
	usages := map[string]string{
		"deploy":  "Usage: previewctl [global flags] deploy [--manifest FILE] [--output text|json] SOURCE\n",
		"list":    "Usage: previewctl [global flags] list [--output table|json]\n",
		"get":     "Usage: previewctl [global flags] get [--output text|json] ID\n",
		"start":   "Usage: previewctl [global flags] start [--output text|json] ID\n",
		"stop":    "Usage: previewctl [global flags] stop [--output text|json] ID\n",
		"delete":  "Usage: previewctl [global flags] delete [--output text|json] ID\n",
		"logs":    "Usage: previewctl [global flags] logs [--tail 1..5000] ID\n",
		"health":  "Usage: previewctl [global flags] health [--output text|json]\n",
		"version": "Usage: previewctl version [--check] [--output text|json]\n",
		"update":  "Usage: previewctl update [--check] [--force] [--output text|json]\n",
	}
	usage, ok := usages[command]
	if !ok {
		return usageError(fmt.Sprintf("unknown command %q", command), rootUsage)
	}
	fmt.Fprint(a.streams.Out, usage)
	return nil
}

func newCommandFlags(name string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	return flags
}

func parseCommandFlags(flags *flag.FlagSet, args []string, usage string) error {
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return &helpRequest{usage: usage}
		}
		return usageError(err.Error(), usage)
	}
	return nil
}

func validateOutput(output string, valid ...string) error {
	for _, candidate := range valid {
		if output == candidate {
			return nil
		}
	}
	return fmt.Errorf("unsupported output %q (choose %s)", output, strings.Join(valid, " or "))
}

func durationFromEnvironment(name string, fallback time.Duration) (time.Duration, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration (for example, 15m)", name)
	}
	return parsed, nil
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func displayBuildValue(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}

func emptyAsDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

type commandUsageError struct {
	message string
	usage   string
}

type helpRequest struct {
	usage string
}

func (e *helpRequest) Error() string { return "help requested" }

func (e *commandUsageError) Error() string { return e.message }

func usageError(message, usage string) error {
	return &commandUsageError{message: message, usage: usage}
}

var rootUsage = `previewctl manages preview deployments.

Usage:
  previewctl [global flags] COMMAND [command flags]

Commands:
  deploy    Upload a ZIP or package an executable or source directory
  list      List deployments
  get       Show one deployment
  logs      Read deployment logs
  start     Start a stopped deployment
  stop      Stop a deployment
  delete    Delete a deployment and any orchestrator-owned image
  health    Check orchestrator health
  version   Print build information and optionally check for updates
  update    Check for or install the latest previewctl release

Global flags (must appear before COMMAND):
  --api-url URL       API base URL (default PREVIEWCTL_API_URL or http://127.0.0.1:8081)
  --token TOKEN       bearer token (default PREVIEWCTL_TOKEN)
  --timeout DURATION  request timeout (default PREVIEWCTL_TIMEOUT or 15m)

Run "previewctl help COMMAND" for command usage.
`
