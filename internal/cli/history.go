package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
)

const (
	flagEnvironment = "environment"
	flagState       = "state"
	flagKind        = "kind"
	flagCommit      = "commit"
	flagDigest      = "digest"
	flagLimit       = "limit"
	flagCursor      = "cursor"

	defaultHistoryLimit = 50
)

type deploymentInfo struct {
	ID          int64  `json:"id"`
	App         string `json:"app"`
	Environment string `json:"environment"`
	Kind        string `json:"kind"`
	State       string `json:"state"`
	Trigger     struct {
		Source      string `json:"source"`
		RequestedBy string `json:"requested_by"`
	} `json:"trigger"`
	Commit struct {
		Repository string `json:"repository"`
		SHA        string `json:"sha"`
	} `json:"commit"`
	Image struct {
		Repository     string `json:"repository"`
		Tag            string `json:"tag"`
		Digest         string `json:"digest"`
		PreviousDigest string `json:"previous_digest"`
	} `json:"image"`
	GitOps struct {
		Repository string `json:"repository"`
		CommitSHA  string `json:"commit_sha"`
	} `json:"gitops"`
	ArgoCD struct {
		Revision string `json:"revision"`
	} `json:"argocd"`
	ParentID        *int64     `json:"parent_id"`
	RollbackOfID    *int64     `json:"rollback_of_id"`
	SupersededByID  *int64     `json:"superseded_by_id"`
	FailureReason   string     `json:"failure_reason"`
	Migration       bool       `json:"migration"`
	MigrationReason string     `json:"migration_reason"`
	CreatedAt       time.Time  `json:"created_at"`
	StartedAt       *time.Time `json:"started_at"`
	FinishedAt      *time.Time `json:"finished_at"`
}

type deploymentList struct {
	Items      []deploymentInfo `json:"items"`
	NextCursor string           `json:"next_cursor"`
}

type eventInfo struct {
	Type       string          `json:"type"`
	From       string          `json:"from"`
	To         string          `json:"to"`
	Actor      string          `json:"actor"`
	Message    string          `json:"message"`
	Data       json.RawMessage `json:"data"`
	OccurredAt time.Time       `json:"occurred_at"`
}

type gateInfo struct {
	Name       string     `json:"name"`
	Type       string     `json:"type"`
	Phase      string     `json:"phase"`
	Status     string     `json:"status"`
	Message    string     `json:"message"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
}

type deploymentDetail struct {
	Deployment      deploymentInfo   `json:"deployment"`
	Events          []eventInfo      `json:"events"`
	EventsTruncated bool             `json:"events_truncated"`
	Gates           []gateInfo       `json:"gates"`
	Rollbacks       []deploymentInfo `json:"rollbacks"`
	Retries         []deploymentInfo `json:"retries"`
}

type historyOptions struct {
	environment, commit, digest, cursor string
	states, kinds                       []string
	limit                               int
}

func parseDeploymentID(arg string) (int64, error) {
	id, err := strconv.ParseInt(arg, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("deployment id: must be a positive integer, got %q", arg)
	}
	return id, nil
}

func historyQuery(changed func(string) bool, o historyOptions) (url.Values, error) {
	if o.limit < 1 || o.limit > store.MaxPageSize {
		return nil, fmt.Errorf("--%s: must be from 1 to %d, got %d", flagLimit, store.MaxPageSize, o.limit)
	}
	q := url.Values{}
	q.Set(flagLimit, strconv.Itoa(o.limit))
	if changed(flagEnvironment) {
		if strings.TrimSpace(o.environment) == "" {
			return nil, fmt.Errorf("--%s: must not be blank", flagEnvironment)
		}
		q.Set(flagEnvironment, o.environment)
	}
	if changed(flagCommit) {
		sha := strings.ToLower(o.commit)
		if err := core.ValidateCommitSHA(sha); err != nil {
			return nil, fmt.Errorf("--%s: %w", flagCommit, err)
		}
		q.Set(flagCommit, sha)
	}
	if changed(flagDigest) {
		if err := core.Digest(o.digest).Validate(); err != nil {
			return nil, fmt.Errorf("--%s: %w", flagDigest, err)
		}
		q.Set(flagDigest, o.digest)
	}
	for _, s := range o.states {
		state, err := core.ParseState(s)
		if err != nil {
			return nil, fmt.Errorf("--%s: %w", flagState, err)
		}
		q.Add(flagState, string(state))
	}
	for _, k := range o.kinds {
		if !core.Kind(k).Valid() {
			return nil, fmt.Errorf("--%s: unknown kind %q", flagKind, k)
		}
		q.Add(flagKind, k)
	}
	if changed(flagCursor) {
		n, err := strconv.ParseInt(o.cursor, 10, 64)
		if err != nil || n <= 0 || strconv.FormatInt(n, 10) != o.cursor {
			return nil, fmt.Errorf("--%s: must be a positive integer, got %q", flagCursor, o.cursor)
		}
		q.Set(flagCursor, o.cursor)
	}
	return q, nil
}

func newDeploymentsCommand(lookupEnv LookupEnv) *cobra.Command {
	var server, output string
	var opts historyOptions
	cmd := &cobra.Command{
		Use:   "deployments <app>",
		Short: "List the deployment history of an app, newest first",
		Long: "List the deployments of an app, newest first, deploys and rollbacks alike. Narrow the list with --environment, --state, --kind, --commit and --digest. " +
			"One page is printed per invocation; when more deployments exist the next cursor is printed on stderr, and passing it as --cursor prints the next page. " +
			"With -o json the output is the API response, including next_cursor.",
		Example: "  doorstep deployments blog\n" +
			"  doorstep deployments blog --environment prod --state succeeded --limit 1\n" +
			"  doorstep deployments blog --kind rollback -o json\n" +
			"  doorstep deployments blog --cursor 42",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app := args[0]
			if err := config.ValidateAppName(app); err != nil {
				return fmt.Errorf("app name: %w", err)
			}
			if err := validateOutput(output); err != nil {
				return err
			}
			query, err := historyQuery(cmd.Flags().Changed, opts)
			if err != nil {
				return err
			}
			client, err := newAPIClient(cmd.Flags().Changed, server, lookupEnv)
			if err != nil {
				return err
			}
			var raw json.RawMessage
			if err := client.request(cmd.Context(), http.MethodGet, "/api/v1/apps/"+app+"/deployments", query, nil, &raw); err != nil {
				return err
			}
			if output == outputJSON {
				return writeIndentedJSON(cmd.OutOrStdout(), raw)
			}
			var list deploymentList
			if err := json.Unmarshal(raw, &list); err != nil {
				return fmt.Errorf("decode response: %w", err)
			}
			if err := writeDeploymentTable(cmd.OutOrStdout(), app, list.Items); err != nil {
				return err
			}
			if list.NextCursor != "" {
				_, err = fmt.Fprintf(cmd.ErrOrStderr(), "more deployments: rerun with --%s %s\n", flagCursor, list.NextCursor)
			}
			return err
		},
	}
	flags := cmd.Flags()
	addServerFlag(flags, &server)
	flags.StringVar(&opts.environment, flagEnvironment, "", "only deployments to this environment")
	flags.StringSliceVar(&opts.states, flagState, nil, "only deployments in this state (repeat or comma-separate)")
	flags.StringSliceVar(&opts.kinds, flagKind, nil, "only deployments of this kind: deploy or rollback (repeat or comma-separate)")
	flags.StringVar(&opts.commit, flagCommit, "", "only deployments of this full commit SHA")
	flags.StringVar(&opts.digest, flagDigest, "", "only deployments of this image digest")
	flags.IntVar(&opts.limit, flagLimit, defaultHistoryLimit, "deployments per page, 1 to "+strconv.Itoa(store.MaxPageSize))
	flags.StringVar(&opts.cursor, flagCursor, "", "continue after the next_cursor of the previous page")
	flags.StringVarP(&output, flagOutput, "o", outputTable, "output format: table or json")
	return cmd
}

func newDeploymentCommand(lookupEnv LookupEnv) *cobra.Command {
	var server, output string
	cmd := &cobra.Command{
		Use:   "deployment <id>",
		Short: "Show a deployment with its lineage, gates and events",
		Long: "Show one deployment: what triggered it, the commit, image and digests it moved between, the GitOps commit and Argo CD revision, " +
			"the gate results, the event timeline, and the rollbacks and retries that refer to it. With -o json the output is the API response, " +
			"including the event data and gate evidence the table view leaves out.",
		Example: "  doorstep deployment 42\n  doorstep deployment 42 -o json",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseDeploymentID(args[0])
			if err != nil {
				return err
			}
			if err := validateOutput(output); err != nil {
				return err
			}
			client, err := newAPIClient(cmd.Flags().Changed, server, lookupEnv)
			if err != nil {
				return err
			}
			var raw json.RawMessage
			if err := client.do(cmd.Context(), http.MethodGet, "/api/v1/deployments/"+strconv.FormatInt(id, 10), nil, &raw); err != nil {
				return err
			}
			if output == outputJSON {
				return writeIndentedJSON(cmd.OutOrStdout(), raw)
			}
			var detail deploymentDetail
			if err := json.Unmarshal(raw, &detail); err != nil {
				return fmt.Errorf("decode response: %w", err)
			}
			return writeDeploymentDetail(cmd.OutOrStdout(), detail)
		},
	}
	addServerFlag(cmd.Flags(), &server)
	cmd.Flags().StringVarP(&output, flagOutput, "o", outputTable, "output format: table or json")
	return cmd
}

func valueOrDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func timeOrDash(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

func writeDeploymentTable(out io.Writer, app string, items []deploymentInfo) error {
	if len(items) == 0 {
		_, err := fmt.Fprintf(out, "no deployments found for %s\n", app)
		return err
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tENVIRONMENT\tKIND\tSTATE\tCOMMIT\tDIGEST\tSOURCE\tBY\tCREATED\tFINISHED")
	for _, d := range items {
		_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			d.ID, valueOrDash(printable(d.Environment)), valueOrDash(printable(d.Kind)), valueOrDash(printable(d.State)),
			valueOrDash(shortSHA(d.Commit.SHA)), valueOrDash(shortDigest(d.Image.Digest)),
			valueOrDash(printable(d.Trigger.Source)), valueOrDash(printable(d.Trigger.RequestedBy)),
			d.CreatedAt.UTC().Format(time.RFC3339), timeOrDash(d.FinishedAt))
	}
	return w.Flush()
}

func idText(id *int64) string {
	if id == nil {
		return ""
	}
	return strconv.FormatInt(*id, 10)
}

func joinNonEmpty(sep string, parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, sep)
}

func atText(repository, ref string) string {
	return joinNonEmpty("@", repository, ref)
}

func optionalTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func lineageFields(d deploymentInfo) [][2]string {
	migration := ""
	if d.Migration {
		migration = joinNonEmpty(": ", "yes", printable(d.MigrationReason))
	}
	return [][2]string{
		{"trigger", joinNonEmpty(" by ", printable(d.Trigger.Source), printable(d.Trigger.RequestedBy))},
		{"commit", atText(d.Commit.Repository, d.Commit.SHA)},
		{"image", joinNonEmpty(":", d.Image.Repository, d.Image.Tag)},
		{"digest", d.Image.Digest},
		{"previous digest", d.Image.PreviousDigest},
		{"gitops", atText(d.GitOps.Repository, d.GitOps.CommitSHA)},
		{"argocd revision", printable(d.ArgoCD.Revision)},
		{"rollback of", idText(d.RollbackOfID)},
		{"retry of", idText(d.ParentID)},
		{"superseded by", idText(d.SupersededByID)},
		{"migration", migration},
		{"failure", printable(d.FailureReason)},
		{"created", d.CreatedAt.UTC().Format(time.RFC3339)},
		{"started", optionalTime(d.StartedAt)},
		{"finished", optionalTime(d.FinishedAt)},
	}
}

func writeDeploymentDetail(out io.Writer, detail deploymentDetail) error {
	d := detail.Deployment
	header := fmt.Sprintf("deployment %d: %s of %s", d.ID, valueOrDash(printable(d.Kind)), d.App)
	if d.Environment != "" {
		header += " (" + printable(d.Environment) + ")"
	}
	_, _ = fmt.Fprintf(out, "%s %s\n", header, printable(d.State))

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, f := range lineageFields(d) {
		if f[1] != "" {
			_, _ = fmt.Fprintf(w, "  %s:\t%s\n", f[0], f[1])
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	writeSection(out, "GATES", len(detail.Gates), func(w io.Writer) {
		_, _ = fmt.Fprintln(w, "NAME\tPHASE\tTYPE\tSTATUS\tFINISHED\tMESSAGE")
		for _, g := range detail.Gates {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", printable(g.Name), printable(g.Phase), printable(g.Type),
				printable(g.Status), timeOrDash(g.FinishedAt), valueOrDash(printable(g.Message)))
		}
	})
	eventsHeading := "EVENTS"
	if detail.EventsTruncated {
		eventsHeading += " (older events omitted)"
	}
	writeSection(out, eventsHeading, len(detail.Events), func(w io.Writer) {
		_, _ = fmt.Fprintln(w, "TIME\tTYPE\tTRANSITION\tACTOR\tMESSAGE")
		for _, e := range detail.Events {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", e.OccurredAt.UTC().Format(time.RFC3339), printable(e.Type),
				transitionText(e), valueOrDash(printable(e.Actor)), valueOrDash(eventMessage(e)))
		}
	})
	related := func(items []deploymentInfo) func(io.Writer) {
		return func(w io.Writer) {
			_, _ = fmt.Fprintln(w, "ID\tSTATE\tDIGEST\tCREATED")
			for _, r := range items {
				_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\n", r.ID, printable(r.State), valueOrDash(shortDigest(r.Image.Digest)), r.CreatedAt.UTC().Format(time.RFC3339))
			}
		}
	}
	writeSection(out, "ROLLBACKS", len(detail.Rollbacks), related(detail.Rollbacks))
	writeSection(out, "RETRIES", len(detail.Retries), related(detail.Retries))
	return nil
}

func writeSection(out io.Writer, heading string, rows int, body func(io.Writer)) {
	if rows == 0 {
		return
	}
	_, _ = fmt.Fprintf(out, "\n%s\n", heading)
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	body(w)
	_ = w.Flush()
}

func transitionText(e eventInfo) string {
	if e.From == "" && e.To == "" {
		return "-"
	}
	return valueOrDash(printable(e.From)) + " -> " + valueOrDash(printable(e.To))
}

func eventMessage(e eventInfo) string {
	if e.Message != "" {
		return printable(e.Message)
	}
	var data struct {
		Retry struct {
			Attempt     int    `json:"attempt"`
			MaxAttempts int    `json:"max_attempts"`
			Summary     string `json:"summary"`
		} `json:"retry"`
	}
	if len(e.Data) == 0 || json.Unmarshal(e.Data, &data) != nil || data.Retry.Attempt == 0 {
		return ""
	}
	return fmt.Sprintf("retry %d/%d: %s", data.Retry.Attempt, data.Retry.MaxAttempts, printable(data.Retry.Summary))
}
