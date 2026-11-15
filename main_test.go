package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestParseSelector(t *testing.T) {
	got, err := parseSelector("pull_request.synchronize")
	if err != nil {
		t.Fatal(err)
	}
	if got.eventType != "pull_request" || got.action != "synchronize" {
		t.Fatalf("unexpected selector: %#v", got)
	}
	for _, invalid := range []string{"", "pull_request", "push.opened", "pull_request."} {
		if _, err := parseSelector(invalid); err == nil {
			t.Errorf("parseSelector(%q) unexpectedly succeeded", invalid)
		}
	}
	for _, valid := range []string{"pull_request.opened", "pull_request.synchronize", "pull_request.ready_for_review", "push"} {
		if _, err := parseSelector(valid); err != nil {
			t.Errorf("parseSelector(%q): %v", valid, err)
		}
	}
	for _, labeledSelector := range []string{"pull_request.labelled=ready for review", "pull_request.labeled=bug"} {
		got, err := parseSelector(labeledSelector)
		if err != nil || got.action != "labeled" || got.label == "" {
			t.Errorf("parseSelector(%q) = (%#v, %v), want normalized labeled selector", labeledSelector, got, err)
		}
	}
	for _, invalid := range []string{"pull_request.opened=bug", "pull_request.labelled", "pull_request.labeled", "pull_request.labelled=", "push=bug"} {
		if _, err := parseSelector(invalid); err == nil {
			t.Errorf("parseSelector(%q) unexpectedly succeeded", invalid)
		}
	}
}

func TestParseOptionsRequiresSelector(t *testing.T) {
	_, _, err := parseOptions([]string{"123"}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "--event is required") {
		t.Fatalf("got error %v, wanted required selector error", err)
	}
	opts, help, err := parseOptions([]string{"123", "--event=pull_request.opened", "--repo", "owner/repo"}, &bytes.Buffer{})
	if err != nil || help {
		t.Fatalf("parseOptions returned (%v, %v)", err, help)
	}
	if opts.runID != 123 || opts.event != "pull_request.opened" || opts.repo != "owner/repo" {
		t.Fatalf("unexpected parsed options: %#v", opts)
	}
	pushOpts, pushHelp, err := parseOptions([]string{"456", "--event", "push"}, &bytes.Buffer{})
	if err != nil || pushHelp || pushOpts.runID != 456 || pushOpts.event != "push" {
		t.Fatalf("parseOptions for push = (%#v, %v, %v)", pushOpts, pushHelp, err)
	}
	labeledOpts, labeledHelp, err := parseOptions([]string{"789", "--event", "pull_request.labelled=ready for review"}, &bytes.Buffer{})
	if err != nil || labeledHelp || labeledOpts.event != "pull_request.labelled=ready for review" {
		t.Fatalf("parseOptions for labeled event = (%#v, %v, %v)", labeledOpts, labeledHelp, err)
	}
	skipOpts, skipHelp, err := parseOptions([]string{"789", "--event", "pull_request.opened", "--skip-missing-fields"}, &bytes.Buffer{})
	if err != nil || skipHelp || !skipOpts.skipMissing {
		t.Fatalf("parseOptions for skip-missing-fields = (%#v, %v, %v)", skipOpts, skipHelp, err)
	}
}

func TestReorderFlags(t *testing.T) {
	got := reorderFlags([]string{"123", "--event", "pull_request.opened", "--explain"})
	want := []string{"--event", "pull_request.opened", "--explain", "123"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestValidateEventPayload(t *testing.T) {
	selector := selector{eventType: "pull_request", action: "opened"}
	valid := []byte(`{"action":"opened","number":12,"pull_request":{"head":{"sha":"abc"}}}`)
	if err := validateEventPayload(valid, selector); err != nil {
		t.Fatal(err)
	}
	if err := validateEventPayload([]byte(`{"action":"closed","number":12,"pull_request":{}}`), selector); err == nil {
		t.Fatal("expected selector mismatch error")
	}
}

func TestValidateLabeledArtifactMatchesRequestedLabel(t *testing.T) {
	selected := selector{eventType: "pull_request", action: "labeled", label: "bug"}
	if err := validateEventPayload([]byte(`{"action":"labeled","number":1,"label":{"name":"bug"},"pull_request":{}}`), selected); err != nil {
		t.Fatal(err)
	}
	if err := validateEventPayload([]byte(`{"action":"labeled","number":1,"label":{"name":"feature"},"pull_request":{}}`), selected); err == nil {
		t.Fatal("expected artifact label mismatch")
	}
}

func TestValidatePushEventPayload(t *testing.T) {
	selected := selector{eventType: "push"}
	if err := validateEventPayload([]byte(`{"ref":"refs/heads/main","before":"old","after":"new"}`), selected); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{`{}`, `{"ref":"refs/heads/main"}`, `{"ref":"refs/heads/main","after":"sha"}`, `not-json`} {
		if err := validateEventPayload([]byte(invalid), selected); err == nil {
			t.Errorf("validateEventPayload(%q) unexpectedly succeeded", invalid)
		}
	}
}

func TestFindArtifactAcceptsPushEventJSON(t *testing.T) {
	selected := selector{eventType: "push"}
	for _, tc := range []struct {
		name    string
		content string
	}{
		{name: "raw JSON", content: `{"ref":"refs/heads/main","before":"old","after":"new"}`},
		{name: "zip", content: string(makeArchive(t, archiveEntry{name: "event.json", content: `{"ref":"refs/heads/main","before":"old","after":"new"}`}))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := testClient(map[string][]byte{
				runArtifactsPath("owner/repo", 42):   marshalTestJSON(t, artifactList{Artifacts: []artifact{{ID: 8, Name: "debug-logs"}}}),
				artifactArchivePath("owner/repo", 8): []byte(tc.content),
			})
			got, found, err := (service{client: client, stderr: &bytes.Buffer{}}).findArtifact(42, selected)
			if err != nil || !found || !json.Valid(got) {
				t.Fatalf("findArtifact = (%s, %v, %v)", got, found, err)
			}
		})
	}
}

func TestMatchPushEventRequiresRefSHAAndPlausibleTime(t *testing.T) {
	created := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	run := workflowRun{CreatedAt: created, HeadSHA: "new", HeadBranch: "main"}
	makePush := func(id, ref, head string, at time.Time) githubEvent {
		payload, _ := json.Marshal(map[string]string{"ref": ref, "head": head, "before": "old"})
		return githubEvent{ID: id, Type: "PushEvent", CreatedAt: at, Payload: payload}
	}
	events := []githubEvent{
		makePush("match", "refs/heads/main", "new", created.Add(-time.Minute)),
		makePush("wrong-ref", "refs/heads/other", "new", created.Add(-time.Minute)),
		makePush("wrong-sha", "refs/heads/main", "older", created.Add(-time.Minute)),
		makePush("too-old", "refs/heads/main", "new", created.Add(-maxEventCorrelation-time.Second)),
		makePush("future", "refs/heads/main", "new", created.Add(time.Second)),
	}
	got, state := matchPushEvent(events, run)
	if got == nil || got.ID != "match" || state != "matched" {
		t.Fatalf("got (%v, %q), wanted match", got, state)
	}
}

func TestMatchPushEventRejectsAmbiguityAndMissingCorrelation(t *testing.T) {
	created := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	run := workflowRun{CreatedAt: created, HeadSHA: "same", HeadBranch: "main"}
	makePush := func(id string) githubEvent {
		return githubEvent{ID: id, Type: "PushEvent", CreatedAt: created.Add(-time.Minute), Payload: json.RawMessage(`{"ref":"refs/heads/main","head":"same","before":"old"}`)}
	}
	if got, state := matchPushEvent([]githubEvent{makePush("one"), makePush("two")}, run); got != nil || state != "ambiguous" {
		t.Fatalf("got (%v, %q), wanted ambiguous", got, state)
	}
	if got, state := matchPushEvent([]githubEvent{makePush("one")}, workflowRun{CreatedAt: created}); got != nil || state != "none" {
		t.Fatalf("got (%v, %q), wanted no match without SHA/ref", got, state)
	}
}

func TestPushEventPayloadUsesWebhookFieldNames(t *testing.T) {
	got, err := pushWebhookPayload(json.RawMessage(`{"repository_id":4,"push_id":5,"ref":"refs/heads/main","head":"after-sha","before":"before-sha"}`))
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]string
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["ref"] != "refs/heads/main" || payload["after"] != "after-sha" || payload["before"] != "before-sha" {
		t.Fatalf("unexpected converted payload: %s", got)
	}
	if _, exists := payload["head"]; exists {
		t.Fatalf("Events API field name leaked into webhook payload: %s", got)
	}
}

func TestEventFileFromArchive(t *testing.T) {
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	file, err := writer.Create("logs/event.json")
	if err != nil {
		t.Fatal(err)
	}
	const content = `{"action":"opened","number":1,"pull_request":{}}`
	if _, err := file.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := eventFileFromArchive(archive.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Fatalf("got %s, want %s", got, content)
	}
}

func TestEventFileFromRawJSON(t *testing.T) {
	const content = `{"action":"opened","number":1,"pull_request":{}}`
	got, err := eventFileFromArchive([]byte(content))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Fatalf("got %s, want %s", got, content)
	}
}

func TestEventFileFromDebugArtifactActionFilename(t *testing.T) {
	const content = `{"action":"labeled","number":1,"label":{"name":"bug"},"pull_request":{}}`
	archive := makeArchive(t, archiveEntry{name: "debug-artifact/pull_request-labeled.json", content: content}, archiveEntry{name: "debug-artifact/env", content: "GITHUB_TOKEN=redacted"})
	got, err := eventFileFromArchive(archive, selector{eventType: "pull_request", action: "labeled", label: "bug"})
	if err != nil || string(got) != content {
		t.Fatalf("eventFileFromArchive = (%s, %v), want selected event file", got, err)
	}
}

func TestEnvEnabled(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{{"true", true}, {"TRUE", true}, {"1", true}, {"false", false}, {"", false}, {"sometimes", false}} {
		t.Setenv("GH_FETCH_EVENT_TEST_BOOL", tc.value)
		if got := envEnabled("GH_FETCH_EVENT_TEST_BOOL"); got != tc.want {
			t.Errorf("envEnabled(%q) = %v, want %v", tc.value, got, tc.want)
		}
	}
}

func TestEventFileFromArchiveRejectsMissingOrMultipleFiles(t *testing.T) {
	for _, files := range [][]archiveEntry{{{name: "logs.txt", content: "no event"}}, {{name: "event.json", content: "{}"}, {name: "nested/event.json", content: "{}"}}} {
		archive := makeArchive(t, files...)
		if _, err := eventFileFromArchive(archive); err == nil {
			t.Errorf("eventFileFromArchive(%v) unexpectedly succeeded", files)
		}
	}
}

func TestFindArtifactRecognizesOnlySupportedUnexpiredNames(t *testing.T) {
	selected := selector{eventType: "pull_request", action: "opened"}
	payload := []byte(`{"action":"opened","number":1,"pull_request":{"head":{"sha":"abc"}}}`)
	archive := makeArchive(t, archiveEntry{name: "event.json", content: string(payload)})
	artifactsJSON := marshalTestJSON(t, artifactList{Artifacts: []artifact{
		{ID: 1, Name: "debug-logs", Expired: true},
		{ID: 2, Name: "unrelated", Expired: false},
		{ID: 3, Name: "event.json", Expired: false},
	}})
	client := testClient(map[string][]byte{
		runArtifactsPath("owner/repo", 42):   artifactsJSON,
		artifactArchivePath("owner/repo", 3): archive,
	})
	var stderr bytes.Buffer
	got, found, err := (service{client: client, stderr: &stderr}).findArtifact(42, selected)
	if err != nil || !found || !bytes.Equal(got, payload) {
		t.Fatalf("findArtifact = (%s, %v, %v)", got, found, err)
	}
}

func TestFindArtifactAcceptsDebugLogsArchive(t *testing.T) {
	selected := selector{eventType: "pull_request", action: "synchronize"}
	payload := []byte(`{"action":"synchronize","number":3,"pull_request":{}}`)
	client := testClient(map[string][]byte{
		runArtifactsPath("owner/repo", 43):   marshalTestJSON(t, artifactList{Artifacts: []artifact{{ID: 9, Name: "debug-logs"}}}),
		artifactArchivePath("owner/repo", 9): makeArchive(t, archiveEntry{name: "event.json", content: string(payload)}),
	})
	got, found, err := (service{client: client, stderr: &bytes.Buffer{}}).findArtifact(43, selected)
	if err != nil || !found || !bytes.Equal(got, payload) {
		t.Fatalf("findArtifact = (%s, %v, %v)", got, found, err)
	}
}

func TestFindArtifactRejectsMultipleCandidatesAndSelectorConflict(t *testing.T) {
	selected := selector{eventType: "pull_request", action: "opened"}
	t.Run("multiple candidates", func(t *testing.T) {
		client := testClient(map[string][]byte{
			runArtifactsPath("owner/repo", 42): marshalTestJSON(t, artifactList{Artifacts: []artifact{{ID: 1, Name: "debug-logs"}, {ID: 2, Name: "event.json"}}}),
		})
		_, _, err := (service{client: client, stderr: &bytes.Buffer{}}).findArtifact(42, selected)
		if err == nil || !strings.Contains(err.Error(), "multiple recognized") {
			t.Fatalf("got error %v, wanted multiple artifact error", err)
		}
	})
	t.Run("selector conflict", func(t *testing.T) {
		conflicting := []byte(`{"action":"closed","number":1,"pull_request":{}}`)
		client := testClient(map[string][]byte{
			runArtifactsPath("owner/repo", 42):   marshalTestJSON(t, artifactList{Artifacts: []artifact{{ID: 1, Name: "debug-logs"}}}),
			artifactArchivePath("owner/repo", 1): makeArchive(t, archiveEntry{name: "event.json", content: string(conflicting)}),
		})
		_, _, err := (service{client: client, stderr: &bytes.Buffer{}}).findArtifact(42, selected)
		if err == nil || !strings.Contains(err.Error(), "conflicts with --event") {
			t.Fatalf("got error %v, wanted selector conflict", err)
		}
	})
}

func TestMatchEventSurvivesMutablePRHeadInHistoricalRun(t *testing.T) {
	created := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	payload := json.RawMessage(`{"action":"synchronize","number":7,"pull_request":{"head":{"sha":"later-current-head"}}}`)
	run := workflowRun{
		CreatedAt: created,
		HeadSHA:   "head-sha",
		PullRequests: []runPullRef{{Number: 7, Head: struct {
			SHA string `json:"sha"`
		}{SHA: "later-current-head"}}},
	}
	events := []githubEvent{{ID: "evt-1", Type: "PullRequestEvent", CreatedAt: created.Add(-time.Minute), Payload: payload}}
	got, state := matchEvent(events, run, selector{eventType: "pull_request", action: "synchronize"})
	if state != "matched" || got == nil || got.ID != "evt-1" {
		t.Fatalf("got (%v, %q), wanted evt-1 matched", got, state)
	}
	mutatedPR := []githubEvent{{ID: "evt-2", Type: "PullRequestEvent", CreatedAt: created, Payload: json.RawMessage(`{"action":"synchronize","number":7,"pull_request":{"head":{"sha":"different-current-head"}}}`)}}
	if got, _ := matchEvent(mutatedPR, run, selector{eventType: "pull_request", action: "synchronize"}); got == nil {
		t.Fatal("failed to match a unique historical event because its embedded PR object changed")
	}
}

func TestMatchEventAmbiguous(t *testing.T) {
	created := time.Now().UTC()
	run := workflowRun{CreatedAt: created, PullRequests: []runPullRef{{Number: 7}, {Number: 8}}}
	makeEvent := func(id string, number int64) githubEvent {
		payload, _ := json.Marshal(map[string]any{"action": "opened", "number": number, "pull_request": map[string]any{}})
		return githubEvent{ID: id, Type: "PullRequestEvent", CreatedAt: created.Add(-time.Minute), Payload: payload}
	}
	got, state := matchEvent([]githubEvent{makeEvent("one", 7), makeEvent("two", 8)}, run, selector{eventType: "pull_request", action: "opened"})
	if got != nil || state != "ambiguous" {
		t.Fatalf("got (%v, %q), wanted ambiguous", got, state)
	}
}

func TestMatchEventLabelSelectorDisambiguatesLabeledActions(t *testing.T) {
	created := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	run := workflowRun{CreatedAt: created, PullRequests: []runPullRef{{Number: 7}}}
	makeLabeledEvent := func(id, name string) githubEvent {
		payload, _ := json.Marshal(map[string]any{"action": "labeled", "number": 7, "label": map[string]string{"name": name}, "pull_request": map[string]any{}})
		return githubEvent{ID: id, Type: "PullRequestEvent", CreatedAt: created.Add(-time.Minute), Payload: payload}
	}
	events := []githubEvent{makeLabeledEvent("bug", "bug"), makeLabeledEvent("docs", "documentation")}
	if got, state := matchEvent(events, run, selector{eventType: "pull_request", action: "labeled"}); got != nil || state != "ambiguous" {
		t.Fatalf("without label got (%v, %q), wanted ambiguous", got, state)
	}
	got, state := matchEvent(events, run, selector{eventType: "pull_request", action: "labeled", label: "documentation"})
	if got == nil || got.ID != "docs" || state != "matched" {
		t.Fatalf("with label got (%v, %q), wanted docs match", got, state)
	}
}

func TestMatchEventRequiresPRActionAndPlausibleTime(t *testing.T) {
	created := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	run := workflowRun{CreatedAt: created, HeadSHA: "head", PullRequests: []runPullRef{{Number: 7}}}
	makeEvent := func(id, action string, at time.Time, number int64) githubEvent {
		payload, _ := json.Marshal(map[string]any{"action": action, "number": number, "pull_request": map[string]any{"head": map[string]any{"sha": "head"}}})
		return githubEvent{ID: id, Type: "PullRequestEvent", CreatedAt: at, Payload: payload}
	}
	events := []githubEvent{
		makeEvent("wrong-action", "opened", created.Add(-time.Minute), 7),
		makeEvent("wrong-pr", "synchronize", created.Add(-time.Minute), 8),
		makeEvent("too-old", "synchronize", created.Add(-maxEventCorrelation-time.Second), 7),
		makeEvent("future", "synchronize", created.Add(time.Second), 7),
		{ID: "wrong-type", Type: "PushEvent", CreatedAt: created.Add(-time.Minute), Payload: json.RawMessage(`{"action":"synchronize","number":7,"pull_request":{}}`)},
	}
	got, state := matchEvent(events, run, selector{eventType: "pull_request", action: "synchronize"})
	if got != nil || state != "none" {
		t.Fatalf("got (%v, %q), wanted no match", got, state)
	}
}

func TestMatchEventUsesUniqueSHAAsCorroboration(t *testing.T) {
	created := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	run := workflowRun{CreatedAt: created, HeadSHA: "run-head", PullRequests: []runPullRef{{Number: 7}}}
	makeEvent := func(id, sha string) githubEvent {
		payload, _ := json.Marshal(map[string]any{"action": "synchronize", "number": 7, "pull_request": map[string]any{"head": map[string]any{"sha": sha}}})
		return githubEvent{ID: id, Type: "PullRequestEvent", CreatedAt: created.Add(-time.Minute), Payload: payload}
	}
	got, state := matchEvent([]githubEvent{makeEvent("mutated", "current-head"), makeEvent("corroborated", "run-head")}, run, selector{eventType: "pull_request", action: "synchronize"})
	if got == nil || got.ID != "corroborated" || state != "matched" {
		t.Fatalf("got (%v, %q), wanted corroborated match", got, state)
	}
}

func TestFillMissingPreservesHistoricalValues(t *testing.T) {
	var oldFields, newFields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(`{"title":"historical","body":null,"head":{"sha":"old"}}`), &oldFields); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"title":"current","body":"new body","head":{"sha":"new","ref":"branch"}}`), &newFields); err != nil {
		t.Fatal(err)
	}
	fillMissing(oldFields, newFields)
	data, _ := json.Marshal(oldFields)
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got["title"] != "historical" || got["body"] != nil {
		t.Fatalf("unexpected merged values: %s", data)
	}
	head := got["head"].(map[string]any)
	if head["sha"] != "old" || head["ref"] != "branch" {
		t.Fatalf("unexpected nested merged values: %s", data)
	}
}

func TestResolvePullRequestUsesUniqueCommitAssociation(t *testing.T) {
	client := testClient(map[string][]byte{
		commitPullsPath("owner/repo", "head-sha"): []byte(`[{"number":9}]`),
	})
	got, err := (service{client: client, stderr: &bytes.Buffer{}}).resolvePullRequest(workflowRun{HeadSHA: "head-sha"}, nil)
	if err != nil || got != 9 {
		t.Fatalf("resolvePullRequest = (%d, %v), want (9, nil)", got, err)
	}
}

func TestResolvePullRequestCommitFallbackDisambiguatesRunAssociations(t *testing.T) {
	client := testClient(map[string][]byte{
		commitPullsPath("owner/repo", "head-sha"): []byte(`[{"number":10}]`),
	})
	run := workflowRun{HeadSHA: "head-sha", PullRequests: []runPullRef{{Number: 9}, {Number: 10}}}
	got, err := (service{client: client, stderr: &bytes.Buffer{}}).resolvePullRequest(run, nil)
	if err != nil || got != 10 {
		t.Fatalf("resolvePullRequest = (%d, %v), want (10, nil)", got, err)
	}
}

func TestListEventsStopsAtThreeHundredRecords(t *testing.T) {
	page := make([]githubEvent, 100)
	for i := range page {
		page[i] = githubEvent{ID: fmt.Sprint(i)}
	}
	responses := map[string][]byte{
		repoEventsPath("owner/repo", 1): marshalTestJSON(t, page),
		repoEventsPath("owner/repo", 2): marshalTestJSON(t, page),
		repoEventsPath("owner/repo", 3): marshalTestJSON(t, page),
	}
	events, err := (service{client: testClient(responses)}).listEvents()
	if err != nil || len(events) != 300 {
		t.Fatalf("listEvents = (%d, %v), want (300, nil)", len(events), err)
	}
}

func TestResolvePullRequestRejectsAmbiguousCommitAssociation(t *testing.T) {
	client := testClient(map[string][]byte{
		commitPullsPath("owner/repo", "head-sha"): []byte(`[{"number":9},{"number":10}]`),
	})
	_, err := (service{client: client, stderr: &bytes.Buffer{}}).resolvePullRequest(workflowRun{HeadSHA: "head-sha"}, nil)
	if err == nil || !strings.Contains(err.Error(), "multiple pull requests") {
		t.Fatalf("got error %v, wanted ambiguous PR error", err)
	}
}

func TestFetchReconstructsOnMissingOrAmbiguousEvent(t *testing.T) {
	created := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	run := workflowRun{ID: 42, Event: "pull_request", Status: "completed", CreatedAt: created, HeadSHA: "head", PullRequests: []runPullRef{{Number: 7}}}
	currentPR := []byte(`{"number":7,"title":"current title","head":{"sha":"head"}}`)
	for _, tc := range []struct {
		name     string
		events   []githubEvent
		wantWarn string
	}{
		{name: "no event"},
		{name: "ambiguous events", events: []githubEvent{
			makePRTestEvent("event-1", created.Add(-time.Minute), "opened", 7, "head"),
			makePRTestEvent("event-2", created.Add(-2*time.Minute), "opened", 7, "head"),
		}, wantWarn: "multiple Events API records matched"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			responses := map[string][]byte{
				runPath("owner/repo", 42):          marshalTestJSON(t, run),
				runArtifactsPath("owner/repo", 42): []byte(`{"artifacts":[]}`),
				repoEventsPath("owner/repo", 1):    marshalTestJSON(t, tc.events),
				pullRequestPath("owner/repo", 7):   currentPR,
			}
			client := testClient(responses)
			var stderr bytes.Buffer
			result, err := (service{client: client, stderr: &stderr}).fetch(42, selector{eventType: "pull_request", action: "opened"})
			if err != nil {
				t.Fatal(err)
			}
			var payload payloadView
			if err := json.Unmarshal([]byte(result), &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Action != "opened" || payload.Number != 7 {
				t.Fatalf("unexpected reconstruction: %#v", payload)
			}
			if tc.wantWarn != "" && !strings.Contains(stderr.String(), tc.wantWarn) {
				t.Fatalf("stderr %q does not include %q", stderr.String(), tc.wantWarn)
			}
			if !strings.Contains(stderr.String(), "reconstructed") {
				t.Fatalf("expected reconstruction warning, got %q", stderr.String())
			}
		})
	}
}

func TestFetchUsesRequiredSelectorActionWhenEventFeedHasNoMatch(t *testing.T) {
	created := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	run := workflowRun{ID: 42, Event: "pull_request", Status: "completed", CreatedAt: created, HeadSHA: "head", PullRequests: []runPullRef{{Number: 7}}}
	client := testClient(map[string][]byte{
		runPath("owner/repo", 42):          marshalTestJSON(t, run),
		runArtifactsPath("owner/repo", 42): []byte(`{"artifacts":[]}`),
		repoEventsPath("owner/repo", 1):    []byte(`[]`),
		pullRequestPath("owner/repo", 7):   []byte(`{"number":7,"head":{"sha":"head"}}`),
	})
	var stderr bytes.Buffer
	result, err := (service{client: client, stderr: &stderr}).fetch(42, selector{eventType: "pull_request", action: "synchronize"})
	if err != nil {
		t.Fatal(err)
	}
	var payload payloadView
	if err := json.Unmarshal([]byte(result), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Action != "synchronize" || payload.Number != 7 {
		t.Fatalf("selector action was not used: %#v", payload)
	}
	if !strings.Contains(stderr.String(), "action supplied by --event") {
		t.Fatalf("expected user-supplied action provenance, got %q", stderr.String())
	}
}

func TestFetchUsesHistoricalPushEvent(t *testing.T) {
	created := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	run := workflowRun{ID: 42, Event: "push", Status: "completed", CreatedAt: created, HeadSHA: "after-sha", HeadBranch: "main"}
	event := githubEvent{ID: "push-record", Type: "PushEvent", CreatedAt: created.Add(-time.Minute), Actor: runActor{ID: 123, Login: "actor"}, Payload: json.RawMessage(`{"repository_id":1,"push_id":2,"ref":"refs/heads/main","head":"after-sha","before":"before-sha"}`)}
	client := testClient(map[string][]byte{
		runPath("owner/repo", 42):                            marshalTestJSON(t, run),
		runArtifactsPath("owner/repo", 42):                   []byte(`{"artifacts":[]}`),
		repoEventsPath("owner/repo", 1):                      marshalTestJSON(t, []githubEvent{event}),
		repositoryPath("owner/repo"):                         []byte(`{"id":99,"name":"repo","full_name":"owner/repo"}`),
		comparePath("owner/repo", "before-sha", "after-sha"): []byte(`{"status":"ahead","html_url":"https://github.com/owner/repo/compare/before-sha...after-sha","total_commits":1,"commits":[{"sha":"after-sha","html_url":"https://github.com/owner/repo/commit/after-sha","commit":{"author":{"name":"A","email":"a@example.com","date":"2026-11-15T12:00:00Z"},"committer":{"name":"A","email":"a@example.com","date":"2026-11-15T12:00:00Z"},"message":"change","tree":{"sha":"tree-sha"}}}]}`),
	})
	var stderr bytes.Buffer
	result, err := (service{client: client, stderr: &stderr, explain: true}).fetch(42, selector{eventType: "push"})
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(result), &payload); err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	if err := json.Unmarshal([]byte(result), &values); err != nil {
		t.Fatal(err)
	}
	if string(payload["ref"]) != `"refs/heads/main"` || string(payload["before"]) != `"before-sha"` || string(payload["after"]) != `"after-sha"` {
		t.Fatalf("unexpected historical push payload: %s", result)
	}
	if strings.Contains(result, "push_id") || values["repository"] == nil || values["sender"] == nil || values["commits"] == nil || values["head_commit"] == nil || values["compare"] == nil || values["forced"] != false {
		t.Fatalf("expected webhook-shaped enriched payload, got %s", result)
	}
	if !strings.Contains(stderr.String(), "Historical + Current") || !strings.Contains(stderr.String(), "Events API record provides ref, before, and after SHA only") || !strings.Contains(stderr.String(), "pusher.email and commit distinct flags cannot be recovered") {
		t.Fatalf("expected translated payload and provenance warning, result=%s stderr=%s", result, stderr.String())
	}
}

func TestFetchReconstructsSparsePushFromRunMetadata(t *testing.T) {
	created := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	run := workflowRun{ID: 42, Event: "push", Status: "completed", CreatedAt: created, HeadSHA: "after-sha", HeadBranch: "main"}
	client := testClient(map[string][]byte{
		runPath("owner/repo", 42):          marshalTestJSON(t, run),
		runArtifactsPath("owner/repo", 42): []byte(`{"artifacts":[]}`),
		repoEventsPath("owner/repo", 1):    []byte(`[]`),
	})
	var stderr bytes.Buffer
	result, err := (service{client: client, stderr: &stderr, explain: true, skipMissing: true}).fetch(42, selector{eventType: "push"})
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(result), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["ref"] != "refs/heads/main" || payload["after"] != "after-sha" || len(payload) != 2 {
		t.Fatalf("unexpected sparse push payload: %s", result)
	}
	if _, exists := payload["before"]; exists {
		t.Fatalf("invented a before SHA: %s", result)
	}
	if !strings.Contains(stderr.String(), "Source: Current + Assumed") || !strings.Contains(stderr.String(), "before could not be inferred") || !strings.Contains(stderr.String(), "interpreted head_branch") {
		t.Fatalf("expected provenance and limitations, got %q", stderr.String())
	}
}

func TestFetchDoesNotInventPushCreationStateWithoutBeforeSHA(t *testing.T) {
	run := workflowRun{ID: 42, Event: "push", Status: "completed", HeadSHA: "after-sha", HeadBranch: "main"}
	client := testClient(map[string][]byte{
		runPath("owner/repo", 42):          marshalTestJSON(t, run),
		runArtifactsPath("owner/repo", 42): []byte(`{"artifacts":[]}`),
		repoEventsPath("owner/repo", 1):    []byte(`[]`),
	})
	var stderr bytes.Buffer
	result, err := (service{client: client, stderr: &stderr}).fetch(42, selector{eventType: "push"})
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(result), &payload); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"created", "compare", "commits", "head_commit"} {
		if _, ok := payload[key]; ok {
			t.Errorf("invented %s without a known before SHA: %s", key, result)
		}
	}
	if string(payload["deleted"]) != "false" {
		t.Fatalf("expected deleted=false from the known after SHA, got %s", result)
	}
}

func TestFetchInfersPushBeforeFromHeadCommitParentWithWarning(t *testing.T) {
	run := workflowRun{ID: 42, Event: "push", Status: "completed", HeadSHA: "after-sha", HeadBranch: "main"}
	client := testClient(map[string][]byte{
		runPath("owner/repo", 42):                               marshalTestJSON(t, run),
		runArtifactsPath("owner/repo", 42):                      []byte(`{"artifacts":[]}`),
		repoEventsPath("owner/repo", 1):                         []byte(`[]`),
		commitPath("owner/repo", "after-sha"):                   []byte(`{"sha":"after-sha","parents":[{"sha":"likely-before"}]}`),
		repositoryPath("owner/repo"):                            []byte(`{"id":99,"name":"repo","full_name":"owner/repo"}`),
		comparePath("owner/repo", "likely-before", "after-sha"): []byte(`{"status":"ahead","html_url":"https://github.com/owner/repo/compare/likely-before...after-sha","total_commits":1,"commits":[{"sha":"after-sha","html_url":"https://github.com/owner/repo/commit/after-sha","commit":{"author":{"name":"A","email":"a@example.com","date":"2026-11-15T12:00:00Z"},"committer":{"name":"A","email":"a@example.com","date":"2026-11-15T12:00:00Z"},"message":"change","tree":{"sha":"tree-sha"}}}]}`),
	})
	var stderr bytes.Buffer
	result, err := (service{client: client, stderr: &stderr, explain: true}).fetch(42, selector{eventType: "push"})
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(result), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["before"] != "likely-before" || payload["after"] != "after-sha" || payload["commits"] == nil || payload["repository"] == nil {
		t.Fatalf("expected first parent as inferred before: %s", result)
	}
	if !strings.Contains(stderr.String(), "inferred before from the first parent") || !strings.Contains(stderr.String(), "may be wrong when multiple commits") || !strings.Contains(stderr.String(), "before <- first parent") {
		t.Fatalf("expected explicit inferred-field provenance, got %q", stderr.String())
	}
}

func TestFetchPushFailsWhenRequiredRunMetadataMissing(t *testing.T) {
	client := testClient(map[string][]byte{
		runPath("owner/repo", 42):          marshalTestJSON(t, workflowRun{Event: "push", Status: "completed", HeadSHA: "sha"}),
		runArtifactsPath("owner/repo", 42): []byte(`{"artifacts":[]}`),
		repoEventsPath("owner/repo", 1):    []byte(`[]`),
	})
	_, err := (service{client: client, stderr: &bytes.Buffer{}}).fetch(42, selector{eventType: "push"})
	if err == nil || !strings.Contains(err.Error(), "missing head_sha or head_branch") {
		t.Fatalf("got error %v, wanted missing run metadata error", err)
	}
}

func TestFetchRequiresLabelToReconstructAmbiguousLabeledEvent(t *testing.T) {
	run := workflowRun{ID: 42, Event: "pull_request", Status: "completed", PullRequests: []runPullRef{{Number: 7}}}
	client := testClient(map[string][]byte{
		runPath("owner/repo", 42):          marshalTestJSON(t, run),
		runArtifactsPath("owner/repo", 42): []byte(`{"artifacts":[]}`),
		repoEventsPath("owner/repo", 1):    []byte(`[]`),
	})
	_, err := (service{client: client, stderr: &bytes.Buffer{}}).fetch(42, selector{eventType: "pull_request", action: "labeled"})
	if err == nil || !strings.Contains(err.Error(), "pull_request.labelled=LABEL_NAME") {
		t.Fatalf("got error %v, wanted label selector error", err)
	}
}

func TestFetchReconstructsLabeledEventFromExplicitLabel(t *testing.T) {
	run := workflowRun{ID: 42, Event: "pull_request", Status: "completed", PullRequests: []runPullRef{{Number: 7}}}
	client := testClient(map[string][]byte{
		runPath("owner/repo", 42):          marshalTestJSON(t, run),
		runArtifactsPath("owner/repo", 42): []byte(`{"artifacts":[]}`),
		repoEventsPath("owner/repo", 1):    []byte(`[]`),
		pullRequestPath("owner/repo", 7):   []byte(`{"number":7,"labels":[{"name":"bug","color":"ff0000"}]}`),
	})
	var stderr bytes.Buffer
	result, err := (service{client: client, stderr: &stderr, explain: true}).fetch(42, selector{eventType: "pull_request", action: "labeled", label: "bug"})
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Action string `json:"action"`
		Label  struct {
			Name  string `json:"name"`
			Color string `json:"color"`
		} `json:"label"`
	}
	if err := json.Unmarshal([]byte(result), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Action != "labeled" || payload.Label.Name != "bug" || payload.Label.Color != "ff0000" {
		t.Fatalf("unexpected reconstructed labeled event: %s", result)
	}
	if !strings.Contains(stderr.String(), "label.name <- --event selector") {
		t.Fatalf("expected label provenance in explain output: %s", stderr.String())
	}
}

func TestFetchRejectsIncompleteAndNonCompletedRuns(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  workflowRun
		want string
	}{
		{name: "in progress", run: workflowRun{Event: "pull_request", Status: "in_progress"}, want: "only completed runs"},
		{name: "wrong event type", run: workflowRun{Event: "push", Status: "completed"}, want: "does not match run event"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := testClient(map[string][]byte{runPath("owner/repo", 42): marshalTestJSON(t, tc.run)})
			_, err := (service{client: client, stderr: &bytes.Buffer{}}).fetch(42, selector{eventType: "pull_request", action: "opened"})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got error %v, wanted %q", err, tc.want)
			}
		})
	}
}

type archiveEntry struct {
	name    string
	content string
}

func makeArchive(t *testing.T, entries ...archiveEntry) []byte {
	t.Helper()
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	for _, entry := range entries {
		file, err := writer.Create(entry.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte(entry.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func marshalTestJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func testClient(responses map[string][]byte) ghClient {
	return ghClient{
		repo: "owner/repo",
		apiFunc: func(path string, _ int) ([]byte, error) {
			response, ok := responses[path]
			if !ok {
				return nil, fmt.Errorf("unexpected API call %s", path)
			}
			return response, nil
		},
	}
}

func makePRTestEvent(id string, created time.Time, action string, number int64, headSHA string) githubEvent {
	payload, err := json.Marshal(map[string]any{
		"action":       action,
		"number":       number,
		"pull_request": map[string]any{"head": map[string]any{"sha": headSHA}},
	})
	if err != nil {
		panic(err)
	}
	return githubEvent{ID: id, Type: "PullRequestEvent", CreatedAt: created, Actor: runActor{ID: 123, Login: "actor"}, Payload: payload}
}

func TestEventPayloadValidationRejectsMissingFields(t *testing.T) {
	selected := selector{eventType: "pull_request", action: "opened"}
	for _, value := range []string{`not json`, `{"action":"opened"}`, `{"action":"opened","number":0,"pull_request":{}}`, `{"action":"opened","number":1,"pull_request":null}`} {
		if err := validateEventPayload([]byte(value), selected); err == nil {
			t.Errorf("validateEventPayload(%q) unexpectedly succeeded", value)
		}
	}
}

func TestFetchUsesEventPayloadAndFetchMissingOnlyAddsAbsentFields(t *testing.T) {
	created := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	run := workflowRun{ID: 42, Event: "pull_request", Status: "completed", CreatedAt: created, HeadSHA: "historical-head", PullRequests: []runPullRef{{Number: 7, Head: struct {
		SHA string `json:"sha"`
	}{SHA: "later-head"}}}}
	event := makePRTestEvent("event-1", created.Add(-time.Minute), "opened", 7, "later-head")
	var eventObject map[string]json.RawMessage
	if err := json.Unmarshal(event.Payload, &eventObject); err != nil {
		t.Fatal(err)
	}
	var prObject map[string]json.RawMessage
	if err := json.Unmarshal(eventObject["pull_request"], &prObject); err != nil {
		t.Fatal(err)
	}
	prObject["title"] = json.RawMessage(`"event-record-title"`)
	eventObject["pull_request"], _ = json.Marshal(prObject)
	event.Payload, _ = json.Marshal(eventObject)
	currentPR := []byte(`{"title":"current-title","body":"current-body","head":{"sha":"latest-head","ref":"feature"}}`)
	responses := map[string][]byte{
		runPath("owner/repo", 42):          marshalTestJSON(t, run),
		runArtifactsPath("owner/repo", 42): []byte(`{"artifacts":[]}`),
		repoEventsPath("owner/repo", 1):    marshalTestJSON(t, []githubEvent{event}),
		pullRequestPath("owner/repo", 7):   currentPR,
		repositoryPath("owner/repo"):       []byte(`{"id":99,"name":"repo","full_name":"owner/repo"}`),
	}
	client := testClient(responses)
	var stderr bytes.Buffer
	result, err := (service{client: client, stderr: &stderr, explain: true}).fetch(42, selector{eventType: "pull_request", action: "opened"})
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Action      string         `json:"action"`
		Number      int64          `json:"number"`
		PullRequest map[string]any `json:"pull_request"`
	}
	if err := json.Unmarshal([]byte(result), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Action != "opened" || decoded.Number != 7 || decoded.PullRequest["title"] != "event-record-title" || decoded.PullRequest["body"] != "current-body" {
		t.Fatalf("unexpected merged event: %#v", decoded)
	}
	var topLevel map[string]json.RawMessage
	if err := json.Unmarshal([]byte(result), &topLevel); err != nil {
		t.Fatal(err)
	}
	if topLevel["repository"] == nil || topLevel["sender"] == nil || topLevel["actor"] != nil {
		t.Fatalf("missing webhook-shaped repository/sender fields or leaked actor envelope: %s", result)
	}
	if !strings.Contains(stderr.String(), "pull_request.body") || !strings.Contains(stderr.String(), "repository") || !strings.Contains(stderr.String(), "current PR state") {
		t.Fatalf("explain output lacks provenance: %s", stderr.String())
	}
}

func TestFetchArtifactsRemainUntouchedByFetchMissingFields(t *testing.T) {
	payload := []byte(`{"action":"opened","number":7,"pull_request":{"head":{"sha":"head"}}}`)
	client := testClient(map[string][]byte{
		runPath("owner/repo", 42):            marshalTestJSON(t, workflowRun{Event: "pull_request", Status: "completed"}),
		runArtifactsPath("owner/repo", 42):   marshalTestJSON(t, artifactList{Artifacts: []artifact{{ID: 1, Name: "event.json"}}}),
		artifactArchivePath("owner/repo", 1): makeArchive(t, archiveEntry{name: "event.json", content: string(payload)}),
	})
	result, err := (service{client: client, stderr: &bytes.Buffer{}}).fetch(42, selector{eventType: "pull_request", action: "opened"})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal([]byte(result), &decoded); err != nil {
		t.Fatal(err)
	}
	if _, exists := decoded["repository"]; exists {
		t.Fatalf("artifact payload was changed: %s", result)
	}
}

func TestFetchSkipArtifactUsesEventsAPI(t *testing.T) {
	created := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	run := workflowRun{Event: "pull_request", Status: "completed", CreatedAt: created, PullRequests: []runPullRef{{Number: 7}}}
	event := makePRTestEvent("event-1", created.Add(-time.Minute), "opened", 7, "head")
	client := ghClient{repo: "owner/repo", apiFunc: func(path string, _ int) ([]byte, error) {
		switch path {
		case runPath("owner/repo", 42):
			return marshalTestJSON(t, run), nil
		case repoEventsPath("owner/repo", 1):
			return marshalTestJSON(t, []githubEvent{event}), nil
		case runArtifactsPath("owner/repo", 42):
			return nil, errors.New("artifact lookup should have been skipped")
		default:
			return nil, fmt.Errorf("unexpected API call %s", path)
		}
	}}
	var stderr bytes.Buffer
	result, err := (service{client: client, stderr: &stderr, skipArtifact: true, skipMissing: true}).fetch(42, selector{eventType: "pull_request", action: "opened"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "Events API record event-1") || !strings.Contains(result, `"action": "opened"`) {
		t.Fatalf("expected Events API payload, got result=%s stderr=%s", result, stderr.String())
	}
}

func TestFetchSkipAPIStateLeavesHistoricalEventsPayloadUnchanged(t *testing.T) {
	created := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	run := workflowRun{Event: "pull_request", Status: "completed", CreatedAt: created, PullRequests: []runPullRef{{Number: 7}}}
	event := makePRTestEvent("event-1", created.Add(-time.Minute), "opened", 7, "head")
	client := ghClient{repo: "owner/repo", apiFunc: func(path string, _ int) ([]byte, error) {
		switch path {
		case runPath("owner/repo", 42):
			return marshalTestJSON(t, run), nil
		case runArtifactsPath("owner/repo", 42):
			return []byte(`{"artifacts":[]}`), nil
		case repoEventsPath("owner/repo", 1):
			return marshalTestJSON(t, []githubEvent{event}), nil
		default:
			return nil, fmt.Errorf("API state lookup should have been skipped: %s", path)
		}
	}}
	var stderr bytes.Buffer
	result, err := (service{client: client, stderr: &stderr, skipAPI: true}).fetch(42, selector{eventType: "pull_request", action: "opened"})
	if err != nil {
		t.Fatal(err)
	}
	var got, want map[string]json.RawMessage
	if err := json.Unmarshal([]byte(result), &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(event.Payload, &want); err != nil {
		t.Fatal(err)
	}
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if !bytes.Equal(gotJSON, wantJSON) {
		t.Fatalf("expected unaugmented historical payload %s, got %s", event.Payload, result)
	}
}

func TestFetchHistoricalPushSkipMissingFieldsAvoidsAPIEnrichment(t *testing.T) {
	created := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	run := workflowRun{Event: "push", Status: "completed", CreatedAt: created, HeadSHA: "after-sha", HeadBranch: "main"}
	event := githubEvent{ID: "push-record", Type: "PushEvent", CreatedAt: created.Add(-time.Minute), Payload: json.RawMessage(`{"ref":"refs/heads/main","head":"after-sha","before":"before-sha"}`)}
	client := ghClient{repo: "owner/repo", apiFunc: func(path string, _ int) ([]byte, error) {
		switch path {
		case runPath("owner/repo", 42):
			return marshalTestJSON(t, run), nil
		case runArtifactsPath("owner/repo", 42):
			return []byte(`{"artifacts":[]}`), nil
		case repoEventsPath("owner/repo", 1):
			return marshalTestJSON(t, []githubEvent{event}), nil
		default:
			return nil, fmt.Errorf("API enrichment should have been skipped: %s", path)
		}
	}}
	result, err := (service{client: client, stderr: &bytes.Buffer{}, skipMissing: true}).fetch(42, selector{eventType: "push"})
	if err != nil {
		t.Fatal(err)
	}
	if result != "{\n  \"after\": \"after-sha\",\n  \"before\": \"before-sha\",\n  \"ref\": \"refs/heads/main\"\n}" {
		t.Fatalf("expected slim translated payload with no enrichment, got %s", result)
	}
}

func TestFetchSkipEventAndAPIStateFailsWithoutCallingEventFeed(t *testing.T) {
	calledEvents := false
	client := ghClient{repo: "owner/repo", apiFunc: func(path string, _ int) ([]byte, error) {
		switch path {
		case runPath("owner/repo", 42):
			return []byte(`{"event":"pull_request","status":"completed"}`), nil
		case runArtifactsPath("owner/repo", 42):
			return []byte(`{"artifacts":[]}`), nil
		case repoEventsPath("owner/repo", 1):
			calledEvents = true
			return []byte(`[]`), nil
		default:
			return nil, fmt.Errorf("unexpected API call %s", path)
		}
	}}
	_, err := (service{client: client, stderr: &bytes.Buffer{}, skipEvent: true, skipAPI: true}).fetch(42, selector{eventType: "pull_request", action: "opened"})
	if err == nil || !strings.Contains(err.Error(), "GH_FETCH_EVENT_SKIP_API") {
		t.Fatalf("got error %v, wanted disabled API reconstruction error", err)
	}
	if calledEvents {
		t.Fatal("repository Events API was called despite skipEvent")
	}
}

func TestResolvePullRequestUsesRunAssociationWithoutCommitLookup(t *testing.T) {
	client := testClient(map[string][]byte{})
	got, err := (service{client: client, stderr: &bytes.Buffer{}}).resolvePullRequest(workflowRun{PullRequests: []runPullRef{{Number: 7}}}, nil)
	if err != nil || got != 7 {
		t.Fatalf("resolvePullRequest = (%d, %v), want (7, nil)", got, err)
	}
}

func TestResolvePullRequestFailsWhenNoAssociationExists(t *testing.T) {
	client := testClient(map[string][]byte{
		commitPullsPath("owner/repo", "head-sha"): []byte(`[]`),
	})
	_, err := (service{client: client, stderr: &bytes.Buffer{}}).resolvePullRequest(workflowRun{HeadSHA: "head-sha"}, nil)
	if err == nil || !strings.Contains(err.Error(), "no pull request association") {
		t.Fatalf("got error %v, wanted missing PR association error", err)
	}
}

func TestFindArtifactDownloadErrorDoesNotSilentlyFallback(t *testing.T) {
	client := testClient(map[string][]byte{
		runArtifactsPath("owner/repo", 42): marshalTestJSON(t, artifactList{Artifacts: []artifact{{ID: 1, Name: "debug-logs"}}}),
	})
	_, _, err := (service{client: client, stderr: &bytes.Buffer{}}).findArtifact(42, selector{eventType: "pull_request", action: "opened"})
	if err == nil || !strings.Contains(err.Error(), "download recognized artifact") {
		t.Fatalf("got error %v, wanted artifact download error", err)
	}
}

func TestMalformedEventPageFailsClosedForEventLookup(t *testing.T) {
	client := testClient(map[string][]byte{repoEventsPath("owner/repo", 1): []byte(`not-json`)})
	if _, err := (service{client: client}).listEvents(); err == nil {
		t.Fatal("expected malformed event page error")
	}
}

func TestFindArtifactTreatsInvalidRecognizedArchiveAsError(t *testing.T) {
	client := testClient(map[string][]byte{
		runArtifactsPath("owner/repo", 42):   marshalTestJSON(t, artifactList{Artifacts: []artifact{{ID: 1, Name: "debug-logs"}}}),
		artifactArchivePath("owner/repo", 1): []byte("not an archive"),
	})
	_, _, err := (service{client: client, stderr: &bytes.Buffer{}}).findArtifact(42, selector{eventType: "pull_request", action: "opened"})
	if err == nil || !strings.Contains(err.Error(), "not a ZIP archive") {
		t.Fatalf("got error %v, wanted invalid archive error", err)
	}
}

func TestResolvePullRequestDoesNotHideCommitAPIErrorWhenNoRunAssociation(t *testing.T) {
	client := ghClient{repo: "owner/repo", apiFunc: func(path string, _ int) ([]byte, error) {
		return nil, errors.New("API unavailable")
	}}
	var stderr bytes.Buffer
	_, err := (service{client: client, stderr: &stderr}).resolvePullRequest(workflowRun{HeadSHA: "head-sha"}, nil)
	if err == nil || !strings.Contains(err.Error(), "no pull request association") {
		t.Fatalf("got error %v, wanted clear missing association error", err)
	}
	if !strings.Contains(stderr.String(), "could not resolve pull requests") {
		t.Fatalf("expected commit API warning, got %q", stderr.String())
	}
}

func TestParseOptionsRejectsNonPositiveRunIDs(t *testing.T) {
	for _, id := range []string{"0", "-1", "not-an-id"} {
		_, _, err := parseOptions([]string{id, "--event", "pull_request.opened"}, &bytes.Buffer{})
		if err == nil {
			t.Errorf("parseOptions(%q) unexpectedly succeeded", id)
		}
	}
}
