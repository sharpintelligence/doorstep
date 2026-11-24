package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/github/githubtest"
	"github.com/sharpintelligence/doorstep/internal/gitops"
	"github.com/sharpintelligence/doorstep/internal/store"
)

const (
	mvpPublicURL = "https://doorstep.example.com"
	mvpPusher    = "octocat"
)

const mvpConfig = `
notifications:
  - name: chat
    type: slack
    webhook_url_env: TEST_SLACK_URL
  - name: hooks
    type: webhook
    url_env: TEST_HOOK_URL
    events: [deploy.queued, deploy.started, deploy.succeeded, deploy.failed, deploy.rolled_back, deploy.cancelled, deploy.superseded]
apps:
  - name: blog
    source:
      repo: example-user/blog
      required_checks: [test]
      deployments: false
    image:
      repository: ghcr.io/example-user/blog
    gitops:
      repo: example-user/homelab
      path: apps/blog
    notify: [chat, hooks]
`

type apiDeployment struct {
	ID      core.DeploymentID `json:"id"`
	App     string            `json:"app"`
	Kind    core.Kind         `json:"kind"`
	State   core.State        `json:"state"`
	Trigger struct {
		Source      core.Source `json:"source"`
		RequestedBy string      `json:"requested_by"`
	} `json:"trigger"`
	Commit struct {
		Repository string `json:"repository"`
		SHA        string `json:"sha"`
	} `json:"commit"`
	Image struct {
		Repository     string      `json:"repository"`
		Tag            string      `json:"tag"`
		Digest         core.Digest `json:"digest"`
		PreviousDigest core.Digest `json:"previous_digest"`
	} `json:"image"`
	GitOps struct {
		Repository string `json:"repository"`
		CommitSHA  string `json:"commit_sha"`
	} `json:"gitops"`
	ArgoCD struct {
		Revision string `json:"revision"`
	} `json:"argocd"`
	RollbackOfID *core.DeploymentID `json:"rollback_of_id"`
	StartedAt    *time.Time         `json:"started_at"`
	FinishedAt   *time.Time         `json:"finished_at"`
}

type apiEvent struct {
	ID   int64          `json:"id"`
	Type core.EventType `json:"type"`
	To   core.State     `json:"to"`
}

type apiDeploymentDetail struct {
	Deployment apiDeployment `json:"deployment"`
	Events     []apiEvent    `json:"events"`
}

func (d apiDeploymentDetail) transitions() []core.State {
	events := slices.Clone(d.Events)
	slices.SortFunc(events, func(a, b apiEvent) int { return int(a.ID - b.ID) })
	var states []core.State
	for _, e := range events {
		if e.Type == core.EventTransition {
			states = append(states, e.To)
		}
	}
	return states
}

type slackPost struct {
	path string
	text string
}

func (p slackPost) mentions(parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(p.text, part) {
			return false
		}
	}
	return true
}

func slackPosts(t *testing.T, e *endpoint) []slackPost {
	t.Helper()
	var posts []slackPost
	for _, r := range e.got() {
		var msg struct {
			Text        string `json:"text"`
			Attachments []struct {
				Blocks []struct {
					Text *struct {
						Text string `json:"text"`
					} `json:"text"`
					Fields []struct {
						Text string `json:"text"`
					} `json:"fields"`
					Elements []struct {
						Text string `json:"text"`
					} `json:"elements"`
				} `json:"blocks"`
			} `json:"attachments"`
		}
		if err := json.Unmarshal(r.body, &msg); err != nil {
			t.Fatalf("slack body %q: %v", r.body, err)
		}
		parts := []string{msg.Text}
		for _, a := range msg.Attachments {
			for _, b := range a.Blocks {
				if b.Text != nil {
					parts = append(parts, b.Text.Text)
				}
				for _, f := range b.Fields {
					parts = append(parts, f.Text)
				}
				for _, el := range b.Elements {
					parts = append(parts, el.Text)
				}
			}
		}
		posts = append(posts, slackPost{path: r.path, text: strings.Join(parts, "\n")})
	}
	return posts
}

func awaitSlack(t *testing.T, e *endpoint, what string, match func(slackPost) bool) {
	t.Helper()
	deadline := time.Now().Add(engineWaitLimit)
	for {
		posts := slackPosts(t, e)
		if slices.ContainsFunc(posts, match) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no slack message %s among %+v", what, posts)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (s *engineServe) cli(token string, args ...string) (int, string, string) {
	s.t.Helper()
	env := maps.Clone(s.env)
	env[EnvServerURL] = "http://" + s.addr
	env[EnvToken] = token
	var out, errOut bytes.Buffer
	code := Run(s.t.Context(), args, &out, &errOut, envFrom(env))
	return code, out.String(), errOut.String()
}

func (s *engineServe) getJSON(path string, into any) {
	s.t.Helper()
	code, body := s.getAuthorized(path)
	if code != http.StatusOK {
		s.t.Fatalf("GET %s = %d %s", path, code, body)
	}
	if err := json.Unmarshal([]byte(body), into); err != nil {
		s.t.Fatalf("GET %s: decode %s: %v", path, body, err)
	}
}

func (s *engineServe) deployment(id core.DeploymentID) apiDeploymentDetail {
	s.t.Helper()
	var detail apiDeploymentDetail
	s.getJSON("/api/v1/deployments/"+strconv.FormatInt(int64(id), 10), &detail)
	return detail
}

func (s *engineServe) webhook(event, delivery, body string) {
	s.t.Helper()
	if code, resp := postWebhook(s.t, "http://"+s.addr+"/webhooks/github", event, delivery, intakeTestSecret, body); code != http.StatusOK {
		s.t.Fatalf("%s webhook %s = %d %s", event, delivery, code, resp)
	}
}

func (s *engineServe) trackedCommit(sha string) core.TrackedCommit {
	s.t.Helper()
	commits, err := s.store.TrackedCommits(s.t.Context(), "example-user/blog", sha)
	if err != nil {
		s.t.Fatalf("TrackedCommits: %v", err)
	}
	if len(commits) != 1 {
		s.t.Fatalf("tracked commits for %s = %+v, want one", sha, commits)
	}
	return commits[0]
}

func (s *engineServe) awaitEnqueued(sha string) core.DeploymentID {
	s.t.Helper()
	deadline := time.Now().Add(engineWaitLimit)
	for {
		if c := s.trackedCommit(sha); c.DeploymentID != nil {
			return *c.DeploymentID
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("commit %s was not enqueued\n%s", sha, s.stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func pushPayload(sha string) string {
	return `{"ref":"refs/heads/main","after":"` + sha + `","repository":{"full_name":"example-user/blog"},"sender":{"login":"` + mvpPusher + `"}}`
}

func checkRunPayload(sha string) string {
	return `{"action":"completed","check_run":{"head_sha":"` + sha + `","name":"test","status":"completed","conclusion":"success"},"repository":{"full_name":"example-user/blog"}}`
}

func (s *engineServe) mergeAndBuild(sha string) (core.DeploymentID, core.Digest) {
	s.t.Helper()
	f := s.fixture
	f.github.SetBranch("example-user", "blog", "main", sha)
	f.github.SetCheckRuns("example-user", "blog", sha, githubtest.CheckRun{ID: 1, Name: "test", Status: "in_progress"})
	s.webhook("push", "push-"+sha, pushPayload(sha))
	if c := s.trackedCommit(sha); c.DeploymentID != nil || c.ChecksState != core.ChecksPending || c.Source != core.SourceWebhook || c.PushedBy != mvpPusher {
		s.t.Fatalf("tracked commit after the push = %+v, want pending checks and nothing queued", c)
	}

	digest := core.Digest(f.registry.Push("example-user/blog", sha))
	f.github.SetCheckRuns("example-user", "blog", sha, githubtest.CheckRun{ID: 1, Name: "test", Status: "completed", Conclusion: "success"})
	s.webhook("check_run", "check-run-"+sha, checkRunPayload(sha))
	return s.awaitEnqueued(sha), digest
}

func (s *engineServe) pinCommit(sha string) gitops.Trailers {
	s.t.Helper()
	for _, c := range s.fixture.homelab.Commits("main") {
		if c.SHA == sha {
			return gitops.ParseMessage(c.Message)
		}
	}
	s.t.Fatalf("GitOps commit %q not found", sha)
	return gitops.Trailers{}
}

func (s *engineServe) assertDeployLineage(d apiDeploymentDetail, id core.DeploymentID, sha string, digest, previous core.Digest) {
	s.t.Helper()
	t := s.t
	got := d.Deployment
	if got.ID != id || got.App != "blog" || got.Kind != core.KindDeploy || got.State != core.StateSucceeded {
		t.Errorf("deployment %d = %+v, want a succeeded deploy of blog", id, got)
	}
	if got.Trigger.Source != core.SourceWebhook || got.Trigger.RequestedBy != mvpPusher {
		t.Errorf("deployment %d trigger = %+v, want the webhook push by %s", id, got.Trigger, mvpPusher)
	}
	if got.Commit.Repository != "example-user/blog" || got.Commit.SHA != sha {
		t.Errorf("deployment %d commit = %+v, want example-user/blog@%s", id, got.Commit, sha)
	}
	if got.Image.Repository != s.fixture.registry.Repo("example-user/blog") || got.Image.Tag != sha || got.Image.Digest != digest || got.Image.PreviousDigest != previous {
		t.Errorf("deployment %d image = %+v, want tag %s at %s over %q", id, got.Image, sha, digest, previous)
	}
	s.assertPinned(got, id, core.KindDeploy, sha, digest, previous)
	if got.StartedAt == nil || got.FinishedAt == nil {
		t.Errorf("deployment %d started_at = %v, finished_at = %v, want both set", id, got.StartedAt, got.FinishedAt)
	}
	if transitions := d.transitions(); !slices.Equal(transitions, deploySucceeded) {
		t.Errorf("deployment %d transitions = %v, want %v", id, transitions, deploySucceeded)
	}
}

func (s *engineServe) assertPinned(got apiDeployment, id core.DeploymentID, kind core.Kind, sha string, digest, previous core.Digest) {
	s.t.Helper()
	t := s.t
	if got.GitOps.Repository != "example-user/homelab" || got.GitOps.CommitSHA == "" || got.ArgoCD.Revision != got.GitOps.CommitSHA {
		t.Errorf("deployment %d gitops = %+v, argocd = %+v, want a pin commit in example-user/homelab that Argo CD synced", id, got.GitOps, got.ArgoCD)
		return
	}
	trailers := s.pinCommit(got.GitOps.CommitSHA)
	if trailers.App != "blog" || trailers.Deployment != id || trailers.Kind != kind || trailers.Digest != digest ||
		trailers.PreviousDigest != previous || trailers.Image != got.Image.Repository ||
		trailers.AppCommit != (core.Commit{Repository: "example-user/blog", SHA: sha}) ||
		trailers.URL != mvpPublicURL+"/deployments/"+strconv.FormatInt(int64(id), 10) {
		t.Errorf("pin commit %s trailers = %+v, want %s %d of example-user/blog@%s at %s over %q", got.GitOps.CommitSHA, trailers, kind, id, sha, digest, previous)
	}
}

func (s *engineServe) pinFile() string {
	content, _ := s.fixture.homelab.File("main", enginePinFile)
	return content
}

func TestMVPDeploysAMergedPullRequestAndRollsBack(t *testing.T) {
	t.Parallel()

	f := newEngineFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.followGitOps(ctx, syncedTo)
	slack := newEndpoint(t)
	s := startEngineServeEnv(t, f, mvpConfig, map[string]string{
		"TEST_SLACK_URL":    slack.URL + slackPath,
		config.EnvPublicURL: mvpPublicURL,
	})
	firstSHA, secondSHA := strings.Repeat("a", 40), strings.Repeat("b", 40)

	if code, body := postWebhook(t, "http://"+s.addr+"/webhooks/github", "push", "forged", "not-the-secret", pushPayload(firstSHA)); code != http.StatusUnauthorized {
		t.Fatalf("forged push = %d %s, want 401", code, body)
	}
	if commits, err := s.store.TrackedCommits(t.Context(), "example-user/blog", firstSHA); err != nil || len(commits) != 0 {
		t.Fatalf("tracked commits after a forged push = %+v, %v, want none", commits, err)
	}

	first, firstDigest := s.mergeAndBuild(firstSHA)
	if d := s.awaitState(first, core.StateSucceeded, core.StateFailed); d.State != core.StateSucceeded {
		t.Fatalf("first deployment = %+v, want succeeded", d)
	}
	firstPin, _ := f.homelab.BranchSHA("main")

	second, secondDigest := s.mergeAndBuild(secondSHA)
	if d := s.awaitState(second, core.StateSucceeded, core.StateFailed); d.State != core.StateSucceeded {
		t.Fatalf("second deployment = %+v, want succeeded", d)
	}
	secondPin, _ := f.homelab.BranchSHA("main")
	if !strings.Contains(s.pinFile(), "digest: "+string(secondDigest)) {
		t.Errorf("pin file does not hold %s:\n%s", secondDigest, s.pinFile())
	}

	firstDetail := s.deployment(first)
	s.assertDeployLineage(firstDetail, first, firstSHA, firstDigest, "")
	if firstDetail.Deployment.GitOps.CommitSHA != firstPin {
		t.Errorf("first deployment pin commit = %s, want GitOps HEAD %s", firstDetail.Deployment.GitOps.CommitSHA, firstPin)
	}
	secondDetail := s.deployment(second)
	s.assertDeployLineage(secondDetail, second, secondSHA, secondDigest, firstDigest)
	if secondDetail.Deployment.GitOps.CommitSHA != secondPin {
		t.Errorf("second deployment pin commit = %s, want GitOps HEAD %s", secondDetail.Deployment.GitOps.CommitSHA, secondPin)
	}

	secondURL := mvpPublicURL + "/deployments/" + strconv.FormatInt(int64(second), 10)
	awaitSlack(t, slack, "for the succeeded second deploy", func(p slackPost) bool {
		return p.path == slackPath && p.mentions(
			"blog: deploy #"+strconv.FormatInt(int64(second), 10)+" succeeded",
			"<"+secondURL+"|View deployment>",
			"/example-user/blog/commit/"+secondSHA+"|",
		)
	})

	deployer := s.token(core.RoleDeployer)
	code, out, errOut := s.cli(deployer, "rollback", "blog", "--reason", "checkout errors", "-o", "json")
	if code != 0 {
		t.Fatalf("doorstep rollback = %d: %s", code, errOut)
	}
	var rolled rollbackInfo
	if err := json.Unmarshal([]byte(out), &rolled); err != nil {
		t.Fatalf("decode rollback output %q: %v", out, err)
	}
	if rolled.Deployment.ID == 0 || rolled.Deployment.Kind != string(core.KindRollback) || rolled.RolledBack.ID != int64(second) ||
		rolled.Block == nil || rolled.Block.Digest != string(secondDigest) || rolled.Block.Reason != "checkout errors" {
		t.Fatalf("rollback output = %+v, want a rollback of %d blocking %s", rolled, second, secondDigest)
	}
	rollbackID := core.DeploymentID(rolled.Deployment.ID)
	if d := s.awaitState(rollbackID, core.StateSucceeded, core.StateFailed); d.State != core.StateSucceeded {
		t.Fatalf("rollback = %+v, want succeeded", d)
	}

	rb := s.deployment(rollbackID).Deployment
	if rb.Kind != core.KindRollback || rb.State != core.StateSucceeded || rb.RollbackOfID == nil || *rb.RollbackOfID != second ||
		rb.Image.Digest != firstDigest || rb.Image.PreviousDigest != secondDigest {
		t.Errorf("rollback deployment = %+v, want a succeeded rollback of %d to %s", rb, second, firstDigest)
	}
	if rb.Trigger.RequestedBy == "" || rb.Trigger.RequestedBy != rolled.Block.CreatedBy {
		t.Errorf("rollback requested_by = %q, want the token actor %q", rb.Trigger.RequestedBy, rolled.Block.CreatedBy)
	}
	s.assertPinned(rb, rollbackID, core.KindRollback, firstSHA, firstDigest, secondDigest)
	if head, _ := f.homelab.BranchSHA("main"); rb.GitOps.CommitSHA != head {
		t.Errorf("rollback pin commit = %s, want GitOps HEAD %s", rb.GitOps.CommitSHA, head)
	}
	if trailers := s.pinCommit(rb.GitOps.CommitSHA); trailers.RollbackOf != second || !slices.Contains(trailers.Blocks, secondDigest) {
		t.Errorf("rollback pin commit trailers = %+v, want rollback of %d blocking %s", trailers, second, secondDigest)
	}
	if !strings.Contains(s.pinFile(), "digest: "+string(firstDigest)) {
		t.Errorf("pin file does not hold %s after the rollback:\n%s", firstDigest, s.pinFile())
	}

	var history struct {
		Items []apiDeployment `json:"items"`
	}
	s.getJSON("/api/v1/apps/blog/deployments", &history)
	var ids []core.DeploymentID
	for _, d := range history.Items {
		ids = append(ids, d.ID)
	}
	if want := []core.DeploymentID{rollbackID, second, first}; !slices.Equal(ids, want) {
		t.Errorf("GET /api/v1/apps/blog/deployments = %v, want %v newest first", ids, want)
	}

	code, out, errOut = s.cli(deployer, "deployments", "blog", "-o", "json", "--kind", "rollback")
	if code != 0 {
		t.Fatalf("doorstep deployments = %d: %s", code, errOut)
	}
	var cliHistory struct {
		Items []apiDeployment `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &cliHistory); err != nil {
		t.Fatalf("decode deployments output %q: %v", out, err)
	}
	if len(cliHistory.Items) != 1 || cliHistory.Items[0].ID != rollbackID || cliHistory.Items[0].RollbackOfID == nil || *cliHistory.Items[0].RollbackOfID != second {
		t.Errorf("doorstep deployments --kind rollback = %+v, want only rollback %d of %d", cliHistory.Items, rollbackID, second)
	}
	code, out, errOut = s.cli(deployer, "deployment", strconv.FormatInt(int64(rollbackID), 10))
	if code != 0 {
		t.Fatalf("doorstep deployment = %d: %s", code, errOut)
	}
	wantHeader := "deployment " + strconv.FormatInt(int64(rollbackID), 10) + ": rollback of blog"
	if !strings.HasPrefix(out, wantHeader) || !strings.Contains(out, string(firstDigest)) || !strings.Contains(out, string(secondDigest)) {
		t.Errorf("doorstep deployment output =\n%s\nwant header %q and digests %s and %s", out, wantHeader, firstDigest, secondDigest)
	}

	awaitSlack(t, slack, "for the rollback", func(p slackPost) bool {
		return p.mentions(
			"blog: deploy #"+strconv.FormatInt(int64(second), 10)+" rolled back",
			"<"+secondURL+"|",
			"<"+mvpPublicURL+"/deployments/"+strconv.FormatInt(int64(rollbackID), 10)+"|View deployment>",
		)
	})

	code, out, errOut = s.cli(deployer, "blocks", "blog", "-o", "json")
	var blocks blockList
	if code != 0 || json.Unmarshal([]byte(out), &blocks) != nil || len(blocks.Blocks) != 1 || blocks.Blocks[0].Digest != string(secondDigest) || !blocks.Blocks[0].RecordedInGit {
		t.Errorf("doorstep blocks blog = %d %q %q, want the block of %s recorded in git", code, out, errOut, secondDigest)
	}
	code, out, errOut = s.cli(deployer, "deploy", "blog", secondSHA)
	if code == 0 || !strings.Contains(errOut, "is blocked by "+rolled.Block.CreatedBy+": checkout errors") {
		t.Errorf("doorstep deploy blog %s = %d %q %q, want it refused by the block", secondSHA, code, out, errOut)
	}
	s.webhook("check_run", "check-run-again-"+secondSHA, checkRunPayload(secondSHA))
	page, err := s.store.ListDeployments(t.Context(), store.DeploymentFilter{App: "blog"})
	if err != nil || len(page.Items) != 3 {
		t.Errorf("deployments after the blocked deploy and a redelivered check_run = %d, %v, want 3", len(page.Items), err)
	}
	if !strings.Contains(s.pinFile(), "digest: "+string(firstDigest)) {
		t.Errorf("pin file left %s after the blocked deploy and a redelivered check_run", firstDigest)
	}

	s.sink.await(t, first, []string{"deploy.queued", "deploy.started", "deploy.succeeded"})
	s.sink.await(t, second, []string{"deploy.queued", "deploy.started", "deploy.succeeded"})
	s.sink.await(t, rollbackID, []string{"deploy.queued", "deploy.started", "deploy.rolled_back"})
	s.stop()
}
