package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

type options struct {
	runID       int64
	event       string
	repo        string
	hostname    string
	output      string
	explain     bool
	skipMissing bool
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "gh-fetch-event:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	opts, help, err := parseOptions(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if help {
		return nil
	}
	selector, err := parseSelector(opts.event)
	if err != nil {
		return err
	}
	client := ghClient{hostname: opts.hostname, repo: opts.repo}
	if err := client.resolveContext(); err != nil {
		return err
	}
	service := service{
		client:       client,
		stderr:       stderr,
		explain:      opts.explain,
		skipMissing:  opts.skipMissing,
		skipArtifact: envEnabled("GH_FETCH_EVENT_SKIP_ARTIFACT"),
		skipEvent:    envEnabled("GH_FETCH_EVENT_SKIP_EVENT"),
		skipAPI:      envEnabled("GH_FETCH_EVENT_SKIP_API"),
	}
	payload, err := service.fetch(opts.runID, selector)
	if err != nil {
		return err
	}

	if opts.output == "" || opts.output == "-" {
		_, err = io.WriteString(stdout, payload+"\n")
		return err
	}
	if err := os.WriteFile(opts.output, []byte(payload+"\n"), 0o644); err != nil {
		return fmt.Errorf("write output file: %w", err)
	}
	return nil
}

func envEnabled(name string) bool {
	value, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(name)))
	return err == nil && value
}

func parseOptions(args []string, stderr io.Writer) (options, bool, error) {
	var opts options
	fs := flag.NewFlagSet("gh fetch-event", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&opts.event, "event", "", "Required selector: pull_request.<action> (labels: pull_request.labelled=NAME) or push")
	fs.StringVar(&opts.repo, "repo", "", "Repository in OWNER/REPO or HOST/OWNER/REPO form (defaults to the current repository)")
	fs.StringVar(&opts.hostname, "hostname", "", "GitHub hostname (defaults to the current gh host)")
	fs.StringVar(&opts.output, "output", "", "Write JSON to a file instead of stdout")
	fs.BoolVar(&opts.explain, "explain", false, "Explain the payload source and field provenance on stderr")
	fs.BoolVar(&opts.skipMissing, "skip-missing-fields", false, "Do not fill missing webhook fields from current API data")

	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		fs.Usage = func() {
			fmt.Fprintln(stderr, "Usage: gh fetch-event <run-id> --event pull_request.<action>|push [flags]")
			fmt.Fprintln(stderr, "")
			fs.PrintDefaults()
		}
		fs.Usage()
		return opts, true, nil
	}
	ordered := reorderFlags(args)
	if err := fs.Parse(ordered); err != nil {
		return opts, false, err
	}
	remaining := fs.Args()
	if len(remaining) != 1 {
		return opts, false, fmt.Errorf("expected one workflow run ID\nUsage: gh fetch-event <run-id> --event pull_request.<action>|push [flags]")
	}
	runID, err := strconv.ParseInt(remaining[0], 10, 64)
	if err != nil || runID <= 0 {
		return opts, false, fmt.Errorf("invalid workflow run ID %q", remaining[0])
	}
	opts.runID = runID
	if opts.event == "" {
		return opts, false, errors.New("--event is required (for example: --event pull_request.opened)")
	}
	return opts, false, nil
}

// The standard flag package stops parsing at the first positional argument.
// gh users commonly put flags after the run ID, so move positionals to the end.
func reorderFlags(args []string) []string {
	flags := make([]string, 0, len(args))
	positionals := make([]string, 0, 1)
	valueFlags := map[string]bool{"--event": true, "-event": true, "--repo": true, "-repo": true, "--hostname": true, "-hostname": true, "--output": true, "-output": true}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			flags = append(flags, arg)
			key := arg
			if k, _, ok := strings.Cut(arg, "="); ok {
				key = k
			}
			if valueFlags[key] && !strings.Contains(arg, "=") && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		positionals = append(positionals, arg)
	}
	return append(flags, positionals...)
}

type selector struct {
	eventType string
	action    string
	label     string
}

func parseSelector(value string) (selector, error) {
	eventPart, labelName, hasLabel := strings.Cut(value, "=")
	if eventPart == "push" && !hasLabel {
		return selector{eventType: "push"}, nil
	}
	parts := strings.Split(eventPart, ".")
	if len(parts) != 2 || parts[0] != "pull_request" || parts[1] == "" {
		return selector{}, fmt.Errorf("supported selectors are pull_request.<action> (use pull_request.labelled=LABEL_NAME for labels) and push; got %q", value)
	}
	action := parts[1]
	if action == "labelled" {
		action = "labeled"
	}
	for _, r := range action {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') {
			return selector{}, fmt.Errorf("invalid pull_request action in selector %q", value)
		}
	}
	if hasLabel && action != "labeled" {
		return selector{}, fmt.Errorf("a label name can only be specified for pull_request.labelled; got %q", value)
	}
	if hasLabel && labelName == "" {
		return selector{}, fmt.Errorf("label name is empty in selector %q", value)
	}
	if action == "labeled" && !hasLabel {
		return selector{}, fmt.Errorf("pull request label events require --event pull_request.labelled=LABEL_NAME; got %q", value)
	}
	return selector{eventType: parts[0], action: action, label: labelName}, nil
}
