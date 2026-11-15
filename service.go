package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	maxEventJSON        = 8 << 20
	maxEventPages       = 3 // GitHub's Events timeline is limited to 300 entries.
	maxEventCorrelation = 6 * time.Hour
)

type service struct {
	client       ghClient
	stderr       io.Writer
	explain      bool
	skipMissing  bool
	skipArtifact bool
	skipEvent    bool
	skipAPI      bool
}

type workflowRun struct {
	ID           int64           `json:"id"`
	Event        string          `json:"event"`
	Status       string          `json:"status"`
	Conclusion   string          `json:"conclusion"`
	CreatedAt    time.Time       `json:"created_at"`
	HeadSHA      string          `json:"head_sha"`
	HeadBranch   string          `json:"head_branch"`
	HeadCommit   json.RawMessage `json:"head_commit"`
	PullRequests []runPullRef    `json:"pull_requests"`
	Actor        runActor        `json:"actor"`
}

type runPullRef struct {
	Number int64 `json:"number"`
	Head   struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

type runActor struct {
	ID        int64  `json:"id,omitempty"`
	Login     string `json:"login,omitempty"`
	AvatarURL string `json:"avatar_url,omitempty"`
	Gravatar  string `json:"gravatar_id,omitempty"`
	URL       string `json:"url,omitempty"`
	HTMLURL   string `json:"html_url,omitempty"`
}

type artifactList struct {
	Artifacts []artifact `json:"artifacts"`
}

type artifact struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Expired bool   `json:"expired"`
}

type githubEvent struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	CreatedAt time.Time       `json:"created_at"`
	Actor     runActor        `json:"actor"`
	Payload   json.RawMessage `json:"payload"`
}

type payloadView struct {
	Action      string          `json:"action"`
	Number      int64           `json:"number"`
	PullRequest json.RawMessage `json:"pull_request"`
	Label       struct {
		Name string `json:"name"`
	} `json:"label"`
}

func (s service) fetch(runID int64, selected selector) (string, error) {
	if selected.eventType == "pull_request" && selected.action == "labeled" && selected.label == "" {
		return "", errors.New("pull request label events require --event pull_request.labelled=LABEL_NAME")
	}
	var run workflowRun
	if err := s.getJSON(runPath(s.client.repo, runID), &run); err != nil {
		return "", fmt.Errorf("get workflow run %d: %w", runID, err)
	}
	if run.Status != "completed" {
		return "", fmt.Errorf("workflow run %d is %q; only completed runs are supported", runID, run.Status)
	}
	if run.Event != selected.eventType {
		return "", fmt.Errorf("selector %q does not match run event %q", selected.eventType+"."+selected.action, run.Event)
	}

	if !s.skipArtifact {
		if raw, found, err := s.findArtifact(runID, selected); err != nil {
			return "", err
		} else if found {
			return prettyJSON(raw)
		}
	}

	var events []githubEvent
	eventState := "none"
	if !s.skipEvent {
		var eventErr error
		events, eventErr = s.listEvents()
		if eventErr != nil {
			fmt.Fprintf(s.stderr, "Warning: could not inspect repository events: %v\n", eventErr)
		}
	}
	var matched *githubEvent
	if !s.skipEvent && selected.eventType == "push" {
		matched, eventState = matchPushEvent(events, run)
	} else if !s.skipEvent {
		matched, eventState = matchEvent(events, run, selected)
	}
	if matched != nil {
		if selected.eventType == "push" {
			payload, err := pushWebhookPayload(matched.Payload)
			if err != nil {
				return "", fmt.Errorf("convert matching PushEvent payload: %w", err)
			}
			var added []string
			if !s.skipMissing && !s.skipAPI {
				payload, added, err = s.augmentPushPayload(payload, matched.Actor, false)
				if err != nil {
					return "", err
				}
			}
			s.reportPushEventMatch(matched.ID, added)
			return prettyJSON(payload)
		}
		var eventPayload map[string]json.RawMessage
		if err := json.Unmarshal(matched.Payload, &eventPayload); err != nil {
			return "", fmt.Errorf("decode matching Events API payload: %w", err)
		}
		var addedFields []string
		if !s.skipMissing && !s.skipAPI {
			var view payloadView
			if err := json.Unmarshal(matched.Payload, &view); err != nil {
				return "", fmt.Errorf("decode matching pull request event: %w", err)
			}
			currentPR, err := s.getPullRequest(view.Number)
			if err != nil {
				return "", fmt.Errorf("fetch current fields for pull request #%d: %w", view.Number, err)
			}
			var historicalPR, currentMap map[string]json.RawMessage
			if err := json.Unmarshal(view.PullRequest, &historicalPR); err != nil {
				return "", fmt.Errorf("decode historical pull request object: %w", err)
			}
			if err := json.Unmarshal(currentPR, &currentMap); err != nil {
				return "", fmt.Errorf("decode current pull request object: %w", err)
			}
			addedFields = fillMissingPaths(historicalPR, currentMap, "pull_request")
			updatedPR, err := json.Marshal(historicalPR)
			if err != nil {
				return "", fmt.Errorf("merge pull request fields: %w", err)
			}
			eventPayload["pull_request"] = updatedPR
			if _, exists := eventPayload["repository"]; !exists {
				repository, err := s.getRepository()
				if err != nil {
					return "", fmt.Errorf("fetch current repository fields: %w", err)
				}
				eventPayload["repository"] = repository
				addedFields = append(addedFields, "repository")
			}
			if _, exists := eventPayload["sender"]; !exists {
				if sender := webhookSender(matched.Actor); len(sender) > 0 {
					eventPayload["sender"] = sender
					addedFields = append(addedFields, "sender")
				}
			}
		}
		result, err := json.Marshal(eventPayload)
		if err != nil {
			return "", fmt.Errorf("encode historical event payload: %w", err)
		}
		s.reportEventMatch(matched.ID, addedFields, selected)
		return prettyJSON(result)
	}
	if selected.eventType == "push" {
		if s.skipAPI {
			return "", errors.New("no artifact or matching event record was found; API-state reconstruction is disabled by GH_FETCH_EVENT_SKIP_API")
		}
		return s.reconstructPush(run, eventState)
	}
	if s.skipAPI {
		return "", errors.New("no artifact or matching event record was found; API-state reconstruction is disabled by GH_FETCH_EVENT_SKIP_API")
	}

	prNumber, err := s.resolvePullRequest(run, events)
	if err != nil {
		return "", err
	}
	currentPR, err := s.getPullRequest(prNumber)
	if err != nil {
		return "", fmt.Errorf("get current pull request #%d: %w", prNumber, err)
	}
	var prObject json.RawMessage = currentPR
	eventPayload := map[string]json.RawMessage{
		"action":       mustJSON(selected.action),
		"number":       mustJSON(prNumber),
		"pull_request": prObject,
	}
	if selected.label != "" {
		labelObject := currentLabel(currentPR, selected.label)
		if len(labelObject) == 0 {
			labelObject = mustJSON(map[string]string{"name": selected.label})
			fmt.Fprintf(s.stderr, "Warning: label %q is absent from the current PR; using only the label name from --event.\n", selected.label)
		}
		eventPayload["label"] = labelObject
	}
	result, err := json.Marshal(eventPayload)
	if err != nil {
		return "", fmt.Errorf("encode reconstructed pull request event: %w", err)
	}
	reason := "no unique Events API record matched the selector and run metadata"
	if eventState == "ambiguous" {
		reason = "multiple Events API records matched; using current pull request data instead"
	}
	fmt.Fprintf(s.stderr, "Warning: %s; reconstructed the event using current pull request data.\n", reason)
	s.reportReconstruction(prNumber, selected.label)
	return prettyJSON(result)
}

func (s service) findArtifact(runID int64, selected selector) ([]byte, bool, error) {
	data, err := s.client.api(runArtifactsPath(s.client.repo, runID), maxJSONResponse)
	if err != nil {
		fmt.Fprintf(s.stderr, "Warning: could not inspect artifacts for run %d: %v\n", runID, err)
		return nil, false, nil
	}
	var listed artifactList
	if err := json.Unmarshal(data, &listed); err != nil {
		return nil, false, fmt.Errorf("decode run artifacts: %w", err)
	}
	candidates := make([]artifact, 0, 2)
	for _, item := range listed.Artifacts {
		if item.Expired {
			continue
		}
		if item.Name == "debug-logs" || item.Name == "event.json" {
			candidates = append(candidates, item)
		}
	}
	if len(candidates) == 0 {
		return nil, false, nil
	}
	if len(candidates) > 1 {
		return nil, false, errors.New("multiple recognized event artifacts are attached to this run")
	}
	archive, err := s.client.api(artifactArchivePath(s.client.repo, candidates[0].ID), maxArtifactArchive)
	if err != nil {
		return nil, false, fmt.Errorf("download recognized artifact %q: %w", candidates[0].Name, err)
	}
	eventJSON, err := eventFileFromArchive(archive, selected)
	if err != nil {
		return nil, false, fmt.Errorf("read event payload from artifact %q: %w", candidates[0].Name, err)
	}
	if err := validateEventPayload(eventJSON, selected); err != nil {
		return nil, false, fmt.Errorf("artifact %q conflicts with --event: %w", candidates[0].Name, err)
	}
	s.reportArtifact(candidates[0].Name)
	return eventJSON, true, nil
}

func eventFileFromArchive(data []byte, selectors ...selector) ([]byte, error) {
	if json.Valid(data) {
		return data, nil
	}
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, errors.New("artifact is not a ZIP archive or raw event JSON")
	}
	var exactMatches, selectedMatches [][]byte
	expectedName := ""
	if len(selectors) > 0 {
		expectedName = debugArtifactEventFilename(selectors[0])
	}
	for _, file := range reader.File {
		if file.FileInfo().IsDir() {
			continue
		}
		isExact := file.Name == "event.json" || strings.HasSuffix(file.Name, "/event.json")
		isSelected := expectedName != "" && (file.Name == expectedName || strings.HasSuffix(file.Name, "/"+expectedName))
		if !isExact && !isSelected {
			continue
		}
		if file.UncompressedSize64 > maxEventJSON {
			return nil, errors.New("event.json in artifact exceeds the size limit")
		}
		body, err := file.Open()
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", file.Name, err)
		}
		content, readErr := io.ReadAll(io.LimitReader(body, maxEventJSON+1))
		closeErr := body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read %s: %w", file.Name, readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close %s: %w", file.Name, closeErr)
		}
		if len(content) > maxEventJSON {
			return nil, errors.New("event.json in artifact exceeds the size limit")
		}
		if isExact {
			exactMatches = append(exactMatches, content)
		} else {
			selectedMatches = append(selectedMatches, content)
		}
	}
	matches := exactMatches
	if len(matches) == 0 {
		matches = selectedMatches
	}
	if len(matches) == 0 {
		return nil, errors.New("recognized artifact does not contain event.json or the selected event payload")
	}
	if len(matches) > 1 {
		return nil, errors.New("artifact contains multiple matching event payload files")
	}
	return matches[0], nil
}

func debugArtifactEventFilename(selected selector) string {
	if selected.eventType == "push" {
		return "push.json"
	}
	if selected.eventType == "pull_request" {
		return "pull_request-" + selected.action + ".json"
	}
	return ""
}

func validateEventPayload(data []byte, selected selector) error {
	if selected.eventType == "push" {
		var view struct {
			Ref    string `json:"ref"`
			Before string `json:"before"`
			After  string `json:"after"`
		}
		if err := json.Unmarshal(data, &view); err != nil {
			return fmt.Errorf("event.json is invalid JSON: %w", err)
		}
		if view.Ref == "" || view.After == "" || view.Before == "" {
			return errors.New("payload is not a usable push event (requires ref, before, and after)")
		}
		return nil
	}
	var view payloadView
	if err := json.Unmarshal(data, &view); err != nil {
		return fmt.Errorf("event.json is invalid JSON: %w", err)
	}
	if view.Action != selected.action {
		return fmt.Errorf("payload action is %q, selector requests %q", view.Action, selected.action)
	}
	if selected.label != "" && view.Label.Name != selected.label {
		return fmt.Errorf("payload label is %q, selector requests %q", view.Label.Name, selected.label)
	}
	if view.Number <= 0 || len(view.PullRequest) == 0 || string(view.PullRequest) == "null" {
		return errors.New("payload is not a usable pull_request event")
	}
	return nil
}

func matchPushEvent(events []githubEvent, run workflowRun) (*githubEvent, string) {
	var candidates []githubEvent
	for _, event := range events {
		if event.Type != "PushEvent" || event.Payload == nil {
			continue
		}
		var payload struct {
			Ref    string `json:"ref"`
			Head   string `json:"head"`
			Before string `json:"before"`
		}
		if json.Unmarshal(event.Payload, &payload) != nil || payload.Ref == "" || payload.Head == "" || payload.Before == "" {
			continue
		}
		if !event.CreatedAt.IsZero() && !run.CreatedAt.IsZero() {
			age := run.CreatedAt.Sub(event.CreatedAt)
			if age < 0 || age > maxEventCorrelation {
				continue
			}
		}
		if run.HeadSHA != "" && payload.Head != run.HeadSHA {
			continue
		}
		if run.HeadBranch != "" && !refMatchesBranch(payload.Ref, run.HeadBranch) {
			continue
		}
		if run.HeadSHA == "" && run.HeadBranch == "" {
			continue
		}
		candidates = append(candidates, event)
	}
	if len(candidates) == 1 {
		return &candidates[0], "matched"
	}
	if len(candidates) > 1 {
		return nil, "ambiguous"
	}
	return nil, "none"
}

func refMatchesBranch(ref, branch string) bool {
	if ref == branch {
		return true
	}
	if strings.HasPrefix(branch, "refs/") {
		return ref == branch
	}
	return ref == "refs/heads/"+branch || ref == "refs/tags/"+branch
}

func pushWebhookPayload(eventPayload json.RawMessage) ([]byte, error) {
	var event struct {
		Ref    string `json:"ref"`
		Head   string `json:"head"`
		Before string `json:"before"`
	}
	if err := json.Unmarshal(eventPayload, &event); err != nil {
		return nil, err
	}
	if event.Ref == "" || event.Head == "" || event.Before == "" {
		return nil, errors.New("Events API PushEvent is missing ref, before, or head")
	}
	return json.Marshal(map[string]string{"ref": event.Ref, "after": event.Head, "before": event.Before})
}

type compareResponse struct {
	Status       string          `json:"status"`
	HTMLURL      string          `json:"html_url"`
	TotalCommits int             `json:"total_commits"`
	Commits      []compareCommit `json:"commits"`
}

type compareCommit struct {
	SHA     string `json:"sha"`
	HTMLURL string `json:"html_url"`
	Commit  struct {
		Author struct {
			Name  string `json:"name"`
			Email string `json:"email"`
			Date  string `json:"date"`
		} `json:"author"`
		Committer struct {
			Name  string `json:"name"`
			Email string `json:"email"`
			Date  string `json:"date"`
		} `json:"committer"`
		Message string `json:"message"`
		Tree    struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	} `json:"commit"`
	Author *struct {
		Login string `json:"login"`
	} `json:"author"`
	Committer *struct {
		Login string `json:"login"`
	} `json:"committer"`
}

func (s service) augmentPushPayload(data []byte, actor runActor, beforeAssumed bool) ([]byte, []string, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, nil, fmt.Errorf("decode push event before augmentation: %w", err)
	}
	var ref, before, after string
	_ = json.Unmarshal(payload["ref"], &ref)
	_ = json.Unmarshal(payload["before"], &before)
	_ = json.Unmarshal(payload["after"], &after)
	var added []string
	setIfMissing := func(key string, value any) error {
		if _, exists := payload[key]; exists {
			return nil
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		payload[key] = encoded
		added = append(added, key)
		return nil
	}

	if _, exists := payload["repository"]; !exists {
		repository, err := s.getRepository()
		if err != nil {
			fmt.Fprintf(s.stderr, "Warning: could not fetch current repository fields for push event: %v\n", err)
		} else {
			payload["repository"] = repository
			added = append(added, "repository")
		}
	}
	if _, exists := payload["sender"]; !exists {
		if sender := webhookSender(actor); len(sender) > 0 {
			payload["sender"] = sender
			added = append(added, "sender")
		}
	}
	if _, exists := payload["pusher"]; !exists && actor.Login != "" {
		if err := setIfMissing("pusher", map[string]string{"name": actor.Login}); err != nil {
			return nil, nil, fmt.Errorf("encode push pusher: %w", err)
		}
	}
	if _, exists := payload["base_ref"]; !exists {
		if err := setIfMissing("base_ref", nil); err != nil {
			return nil, nil, fmt.Errorf("encode push base_ref: %w", err)
		}
	}
	if before != "" {
		if err := setIfMissing("created", isZeroSHA(before)); err != nil {
			return nil, nil, fmt.Errorf("encode push created flag: %w", err)
		}
	}
	if after != "" {
		if err := setIfMissing("deleted", isZeroSHA(after)); err != nil {
			return nil, nil, fmt.Errorf("encode push deleted flag: %w", err)
		}
	}

	if before != "" && after != "" && !isZeroSHA(before) && !isZeroSHA(after) {
		var comparison compareResponse
		if err := s.getJSON(comparePath(s.client.repo, before, after), &comparison); err != nil {
			fmt.Fprintf(s.stderr, "Warning: could not compare push commits %s...%s: %v\n", before, after, err)
		} else {
			if comparison.HTMLURL != "" {
				if err := setIfMissing("compare", comparison.HTMLURL); err != nil {
					return nil, nil, fmt.Errorf("encode push compare URL: %w", err)
				}
			}
			if comparison.Status == "ahead" || comparison.Status == "identical" {
				if err := setIfMissing("forced", false); err != nil {
					return nil, nil, fmt.Errorf("encode push forced flag: %w", err)
				}
			} else if comparison.Status == "behind" || comparison.Status == "diverged" {
				if err := setIfMissing("forced", true); err != nil {
					return nil, nil, fmt.Errorf("encode push forced flag: %w", err)
				}
			}
			if _, exists := payload["commits"]; !exists {
				commits := make([]json.RawMessage, 0, len(comparison.Commits))
				for _, commit := range comparison.Commits {
					mapped, err := commit.webhookPayload()
					if err != nil {
						return nil, nil, err
					}
					commits = append(commits, mapped)
				}
				encoded, err := json.Marshal(commits)
				if err != nil {
					return nil, nil, fmt.Errorf("encode push commits: %w", err)
				}
				payload["commits"] = encoded
				added = append(added, "commits")
				if len(comparison.Commits) > 0 {
					payload["head_commit"] = commits[len(commits)-1]
					added = append(added, "head_commit")
				} else {
					payload["head_commit"] = json.RawMessage("null")
					added = append(added, "head_commit")
				}
				if comparison.TotalCommits > len(comparison.Commits) {
					fmt.Fprintf(s.stderr, "Warning: compare API returned %d of %d commits; push.commits is incomplete.\n", len(comparison.Commits), comparison.TotalCommits)
				}
			}
		}
	}

	if beforeAssumed && len(added) > 0 {
		fmt.Fprintln(s.stderr, "Warning: push details from the compare API depend on the inferred before SHA and may be inaccurate if that SHA was not the previous ref tip.")
	}
	if actor.Login != "" {
		fmt.Fprintln(s.stderr, "Warning: sender and pusher.name use the workflow run actor as an assumption; reruns may have a different actor from the original push. pusher.email is unavailable.")
	}
	if len(added) > 0 {
		fmt.Fprintf(s.stderr, "Added push fields from current API data and run metadata: %s.\n", strings.Join(added, ", "))
	}
	result, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, fmt.Errorf("encode augmented push event: %w", err)
	}
	return result, added, nil
}

func (c compareCommit) webhookPayload() (json.RawMessage, error) {
	person := func(name, email, username string) map[string]string {
		result := make(map[string]string)
		if name != "" {
			result["name"] = name
		}
		if email != "" {
			result["email"] = email
		}
		if username != "" {
			result["username"] = username
		}
		return result
	}
	message := c.Commit.Committer.Date
	if message == "" {
		message = c.Commit.Author.Date
	}
	result := map[string]any{
		"id":        c.SHA,
		"tree_id":   c.Commit.Tree.SHA,
		"message":   c.Commit.Message,
		"timestamp": message,
		"url":       c.HTMLURL,
		"author":    person(c.Commit.Author.Name, c.Commit.Author.Email, loginOf(c.Author)),
		"committer": person(c.Commit.Committer.Name, c.Commit.Committer.Email, loginOf(c.Committer)),
	}
	return json.Marshal(result)
}

func loginOf(user *struct {
	Login string `json:"login"`
}) string {
	if user == nil {
		return ""
	}
	return user.Login
}

func isZeroSHA(sha string) bool {
	if sha == "" {
		return false
	}
	for _, char := range sha {
		if char != '0' {
			return false
		}
	}
	return true
}

func (s service) reconstructPush(run workflowRun, eventState string) (string, error) {
	if run.HeadSHA == "" || run.HeadBranch == "" {
		return "", errors.New("cannot reconstruct push event: workflow run is missing head_sha or head_branch")
	}
	ref := run.HeadBranch
	assumedRef := false
	if !strings.HasPrefix(ref, "refs/") {
		ref = "refs/heads/" + ref
		assumedRef = true
	}
	payload := map[string]json.RawMessage{"ref": mustJSON(ref), "after": mustJSON(run.HeadSHA)}
	var before string
	if !s.skipMissing {
		var commit struct {
			Parents []struct {
				SHA string `json:"sha"`
			} `json:"parents"`
		}
		if err := s.getJSON(commitPath(s.client.repo, run.HeadSHA), &commit); err != nil {
			fmt.Fprintf(s.stderr, "Warning: could not inspect head commit parents for a likely before SHA: %v\n", err)
		} else if len(commit.Parents) > 0 && commit.Parents[0].SHA != "" {
			before = commit.Parents[0].SHA
			payload["before"] = mustJSON(before)
		}
	}
	reason := "no unique historical PushEvent matched the run metadata"
	if eventState == "ambiguous" {
		reason = "multiple historical PushEvent records matched the run metadata"
	}
	fmt.Fprintf(s.stderr, "Warning: %s; reconstructed a push event from workflow run metadata.\n", reason)
	if before != "" {
		fmt.Fprintln(s.stderr, "Warning: inferred before from the first parent of after, assuming it was the ref tip before a single-commit push. This may be wrong when multiple commits were pushed or history was rewritten.")
	} else {
		fmt.Fprintln(s.stderr, "Warning: before could not be inferred from the head commit; it is omitted.")
	}
	if assumedRef {
		fmt.Fprintln(s.stderr, "Warning: interpreted head_branch as a branch name and prefixed refs/heads/; the workflow run metadata does not identify branch versus tag here.")
	}
	result, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode reconstructed push event: %w", err)
	}
	if !s.skipMissing && !s.skipAPI {
		var added []string
		result, added, err = s.augmentPushPayload(result, run.Actor, true)
		if err != nil {
			return "", err
		}
		if s.explain {
			fmt.Fprintf(s.stderr, "Push fields added from current API data or run metadata: %s.\n", strings.Join(added, ", "))
		}
	}
	if s.explain {
		fmt.Fprintln(s.stderr, "Source: Current + Assumed.")
		fmt.Fprintln(s.stderr, "Provenance: after <- workflow run head_sha; ref <- workflow run head_branch (refs/heads/ prefix assumed when absent); before <- first parent of after when available (assumed to be the prior ref tip for a single-commit push); commits/repository <- current GitHub API data; sender/pusher.name <- workflow run actor when present (assumed); created/deleted/forced <- inferred; base_ref <- assumed null. Commit distinct flags and pusher.email are unavailable.")
	}
	return prettyJSON(result)
}

func (s service) listEvents() ([]githubEvent, error) {
	all := make([]githubEvent, 0, maxEventPages*100)
	for page := 1; page <= maxEventPages; page++ {
		data, err := s.client.api(repoEventsPath(s.client.repo, page), maxJSONResponse)
		if err != nil {
			return nil, err
		}
		var current []githubEvent
		if err := json.Unmarshal(data, &current); err != nil {
			return nil, fmt.Errorf("decode repository events page %d: %w", page, err)
		}
		all = append(all, current...)
		if len(current) < 100 {
			break
		}
	}
	return all, nil
}

func matchEvent(events []githubEvent, run workflowRun, selected selector) (*githubEvent, string) {
	runPRNumbers := make(map[int64]struct{}, len(run.PullRequests))
	for _, pr := range run.PullRequests {
		runPRNumbers[pr.Number] = struct{}{}
	}
	type candidate struct {
		event    githubEvent
		shaMatch bool
	}
	var candidates []candidate
	for _, event := range events {
		if event.Type != "PullRequestEvent" || event.Payload == nil {
			continue
		}
		var view payloadView
		if json.Unmarshal(event.Payload, &view) != nil || view.Action != selected.action || view.Number <= 0 || len(view.PullRequest) == 0 {
			continue
		}
		if selected.label != "" && view.Label.Name != selected.label {
			continue
		}
		if !event.CreatedAt.IsZero() && !run.CreatedAt.IsZero() {
			age := run.CreatedAt.Sub(event.CreatedAt)
			if age < 0 || age > maxEventCorrelation {
				continue
			}
		}
		if len(runPRNumbers) > 0 {
			_, found := runPRNumbers[view.Number]
			if !found {
				continue
			}
		}
		if len(runPRNumbers) == 0 && run.Actor.Login != "" && event.Actor.Login != "" && run.Actor.Login != event.Actor.Login {
			continue
		}
		// The Events API's embedded pull_request object can reflect the PR's
		// current state. A matching run head is useful corroboration, but a
		// mismatch is not grounds to reject an otherwise unique action/PR/time
		// match. pull_requests[].head.sha is also mutable for old runs.
		shaMatch := false
		if run.HeadSHA != "" {
			var pr struct {
				Head struct {
					SHA string `json:"sha"`
				} `json:"head"`
			}
			if json.Unmarshal(view.PullRequest, &pr) == nil && pr.Head.SHA != "" && pr.Head.SHA == run.HeadSHA {
				shaMatch = true
			}
		}
		candidates = append(candidates, candidate{event: event, shaMatch: shaMatch})
	}
	if len(candidates) == 1 {
		return &candidates[0].event, "matched"
	}
	if len(candidates) > 1 {
		var shaCandidates []githubEvent
		for _, candidate := range candidates {
			if candidate.shaMatch {
				shaCandidates = append(shaCandidates, candidate.event)
			}
		}
		if len(shaCandidates) == 1 {
			return &shaCandidates[0], "matched"
		}
		return nil, "ambiguous"
	}
	return nil, "none"
}

func (s service) resolvePullRequest(run workflowRun, events []githubEvent) (int64, error) {
	if len(run.PullRequests) == 1 && run.PullRequests[0].Number > 0 {
		return run.PullRequests[0].Number, nil
	}
	if run.HeadSHA != "" {
		data, err := s.client.api(commitPullsPath(s.client.repo, run.HeadSHA), maxJSONResponse)
		if err == nil {
			var prs []struct {
				Number int64 `json:"number"`
			}
			if json.Unmarshal(data, &prs) == nil {
				unique := make(map[int64]struct{})
				for _, pr := range prs {
					if pr.Number > 0 {
						unique[pr.Number] = struct{}{}
					}
				}
				if len(unique) == 1 {
					for number := range unique {
						return number, nil
					}
				}
				if len(unique) > 1 {
					return 0, errors.New("multiple pull requests are associated with the run SHA; cannot reconstruct safely")
				}
			}
		} else {
			fmt.Fprintf(s.stderr, "Warning: could not resolve pull requests from run SHA: %v\n", err)
		}
	}
	if len(run.PullRequests) > 1 {
		return 0, errors.New("workflow run metadata lists multiple pull requests and no unique association was found")
	}
	if len(events) > 0 {
		return 0, errors.New("no unique pull request could be associated with the workflow run")
	}
	return 0, errors.New("workflow run has no pull request association and GitHub could not identify one from its SHA")
}

func (s service) getPullRequest(number int64) (json.RawMessage, error) {
	data, err := s.client.api(pullRequestPath(s.client.repo, number), maxJSONResponse)
	if err != nil {
		return nil, err
	}
	if !json.Valid(data) {
		return nil, errors.New("GitHub returned invalid pull request JSON")
	}
	return json.RawMessage(data), nil
}

func (s service) getRepository() (json.RawMessage, error) {
	data, err := s.client.api(repositoryPath(s.client.repo), maxJSONResponse)
	if err != nil {
		return nil, err
	}
	if !json.Valid(data) {
		return nil, errors.New("GitHub returned invalid repository JSON")
	}
	return json.RawMessage(data), nil
}

func webhookSender(actor runActor) json.RawMessage {
	if actor.Login == "" && actor.ID == 0 {
		return nil
	}
	data, err := json.Marshal(actor)
	if err != nil {
		return nil
	}
	return data
}

func (s service) getJSON(path string, target any) error {
	data, err := s.client.api(apiItemPath(path), maxJSONResponse)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

func fillMissing(destination, source map[string]json.RawMessage) {
	fillMissingPaths(destination, source, "")
}

func fillMissingPaths(destination, source map[string]json.RawMessage, prefix string) []string {
	var added []string
	for key, incoming := range source {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		current, exists := destination[key]
		if !exists {
			destination[key] = incoming
			added = append(added, path)
			continue
		}
		var currentObject, incomingObject map[string]json.RawMessage
		if json.Unmarshal(current, &currentObject) == nil && json.Unmarshal(incoming, &incomingObject) == nil && currentObject != nil && incomingObject != nil {
			added = append(added, fillMissingPaths(currentObject, incomingObject, path)...)
			merged, err := json.Marshal(currentObject)
			if err == nil {
				destination[key] = merged
			}
		}
	}
	return added
}

func (s service) reportArtifact(name string) {
	fmt.Fprintf(s.stderr, "Source: Original (run artifact %s).\n", name)
	if s.explain {
		fmt.Fprintln(s.stderr, "Provenance: all output fields are from the event payload captured in the artifact.")
	}
}

func (s service) reportEventMatch(eventID string, added []string, selected selector) {
	source := "Historical"
	if len(added) > 0 {
		source = "Historical + Current"
	}
	fmt.Fprintf(s.stderr, "Source: %s (Events API record %s).\n", source, eventID)
	fmt.Fprintln(s.stderr, "Warning: the event action and time are historical, but the embedded pull_request object may reflect current PR state.")
	if s.explain {
		if s.skipAPI {
			fmt.Fprintln(s.stderr, "Provenance: all payload fields <- historical Events API record; actor remains in the Events API envelope and was not copied into sender because API-state enrichment is disabled.")
		} else {
			fmt.Fprintln(s.stderr, "Provenance: action, number, and event time <- historical Events API record; embedded pull_request.* may reflect current PR state; missing pull_request.* <- current PR API response; repository <- current repository API response; sender <- actor on the historical Events API record.")
		}
		if selected.action == "labeled" {
			fmt.Fprintln(s.stderr, "Provenance: label <- matching historical Events API record; the selector's label value filters candidates by label.name.")
		}
		if len(added) > 0 {
			fmt.Fprintf(s.stderr, "Fields added where absent: %s.\n", strings.Join(added, ", "))
		} else if !s.skipMissing {
			fmt.Fprintln(s.stderr, "No fields were missing from the historical event record.")
		}
	}
	if len(added) > 0 {
		fmt.Fprintf(s.stderr, "Added missing webhook fields from current PR/repository data and the Events API actor (%d fields).\n", len(added))
	}
}

func (s service) reportPushEventMatch(eventID string, added []string) {
	source := "Historical"
	if len(added) > 0 {
		source = "Historical + Current"
	}
	fmt.Fprintf(s.stderr, "Source: %s (Events API PushEvent %s).\n", source, eventID)
	fmt.Fprintln(s.stderr, "Warning: the Events API record provides ref, before, and after SHA only; other historical push fields may be unavailable.")
	if len(added) > 0 {
		fmt.Fprintf(s.stderr, "Added push fields from current API data and the Events API actor (%d fields).\n", len(added))
	}
	if s.explain {
		fmt.Fprintln(s.stderr, "Provenance: ref and before <- historical PushEvent payload; after <- historical PushEvent head; repository/commits/compare <- current GitHub API data; sender <- historical Events API actor; pusher.name <- Events API actor (assumed); forced/created/deleted/base_ref may be inferred.")
		if len(added) > 0 {
			fmt.Fprintf(s.stderr, "Fields added where absent: %s. pusher.email and commit distinct flags cannot be recovered from the APIs used.\n", strings.Join(added, ", "))
		}
	}
}

func currentLabel(currentPR json.RawMessage, name string) json.RawMessage {
	var current struct {
		Labels []json.RawMessage `json:"labels"`
	}
	if json.Unmarshal(currentPR, &current) != nil {
		return nil
	}
	for _, label := range current.Labels {
		var item struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(label, &item) == nil && item.Name == name {
			return label
		}
	}
	return nil
}

func (s service) reportReconstruction(number int64, label string) {
	fmt.Fprintf(s.stderr, "Source: Current (reconstructed from pull request #%d; action supplied by --event).\n", number)
	if s.explain {
		fmt.Fprintln(s.stderr, "Provenance: action <- --event selector; number and pull_request.* <- current PR API response.")
		if label != "" {
			fmt.Fprintf(s.stderr, "Provenance: label.name <- --event selector; other label fields <- current PR label when available.\n")
		}
	}
}

func prettyJSON(data []byte) (string, error) {
	if !json.Valid(data) {
		return "", errors.New("resulting event payload is invalid JSON")
	}
	var out bytes.Buffer
	if err := json.Indent(&out, data, "", "  "); err != nil {
		return "", err
	}
	return out.String(), nil
}

func mustJSON(value any) json.RawMessage {
	b, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return b
}
