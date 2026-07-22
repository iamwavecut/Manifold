package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var version = "dev"

const (
	protocolRevision   = 2
	featureScopeGlob   = "retrieval.scope_glob"
	featureSourceTypes = "retrieval.source_types"
	featureTreeGlob    = "tree.glob"
	featureRemember    = "memory.remember.v1"
)

type client struct {
	baseURL  string
	apiKey   string
	http     *http.Client
	meta     serverMetadata
	metaRead bool
}

type apiError struct {
	Status int
	Body   problem
}

type problem struct {
	Title         string `json:"title"`
	Detail        string `json:"detail"`
	Code          string `json:"code"`
	RequestID     string `json:"request_id"`
	SuggestedSlug string `json:"suggested_slug"`
	RetryAfter    string `json:"retry_after"`
	Remediation   struct {
		Summary string   `json:"summary"`
		Steps   []string `json:"steps"`
	} `json:"remediation"`
	Violations []struct {
		Pointer string `json:"pointer"`
		Message string `json:"message"`
		Code    string `json:"code"`
	} `json:"violations"`
	Blockers []struct {
		Resource string `json:"resource"`
		Location string `json:"location"`
		Reason   string `json:"reason"`
	} `json:"blockers"`
	Candidates           []memoryCandidate `json:"candidates,omitempty"`
	DegradedDependencies []string          `json:"degraded_dependencies,omitempty"`
	RequiredFeatures     []string          `json:"required_features,omitempty"`
	ServerVersion        string            `json:"server_version,omitempty"`
}

type serverMetadata struct {
	Service          string   `json:"service"`
	Version          string   `json:"version"`
	Commit           string   `json:"commit"`
	APIMajor         int      `json:"api_major"`
	ProtocolRevision int      `json:"protocol_revision"`
	Features         []string `json:"features"`
}

type memoryCandidate struct {
	ID           string `json:"id"`
	Path         string `json:"path"`
	Title        string `json:"title"`
	Revision     string `json:"revision"`
	CanonicalRef string `json:"canonical_ref"`
	Snippet      string `json:"snippet,omitempty"`
}

type jobState struct {
	ID     string   `json:"id"`
	Status string   `json:"status"`
	Error  *problem `json:"error,omitempty"`
}

var jobPollInterval = 250 * time.Millisecond

func (e *apiError) Error() string {
	if e.Body.Detail != "" {
		return e.Body.Detail
	}
	return e.Body.Code
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	global := flag.NewFlagSet("manifold", flag.ContinueOnError)
	global.SetOutput(stderr)
	jsonOutput := global.Bool("json", false, "print unmodified JSON")
	timeout := global.Duration("timeout", 30*time.Second, "HTTP timeout")
	showVersion := global.Bool("version", false, "print version")
	if err := global.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Fprintf(stdout, "%s protocol=%d\n", version, protocolRevision)
		return 0
	}
	remaining := global.Args()
	if len(remaining) == 0 {
		printUsage(stderr)
		return 2
	}
	baseURL := strings.TrimSpace(os.Getenv("MANIFOLD_URL"))
	apiKey := strings.TrimSpace(os.Getenv("MANIFOLD_API_KEY"))
	if baseURL == "" || apiKey == "" {
		fmt.Fprintln(stderr, "ERROR [local_configuration] MANIFOLD_URL and MANIFOLD_API_KEY must both be set")
		return 2
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		fmt.Fprintln(stderr, "ERROR [local_configuration] MANIFOLD_URL must be an absolute http:// or https:// URL")
		return 2
	}
	c := &client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: *timeout},
	}
	payload, err := execute(c, remaining)
	if err != nil {
		var semantic *apiError
		if errors.As(err, &semantic) {
			if *jsonOutput {
				_ = json.NewEncoder(stderr).Encode(semantic.Body)
			} else {
				renderProblem(stderr, semantic.Body)
			}
			return exitCode(semantic.Body.Code)
		}
		fmt.Fprintf(stderr, "ERROR [local_input] %v\n", err)
		return 2
	}
	if *jsonOutput {
		var pretty bytes.Buffer
		if json.Indent(&pretty, payload, "", "  ") == nil {
			fmt.Fprintln(stdout, pretty.String())
		} else {
			fmt.Fprintln(stdout, string(payload))
		}
		return 0
	}
	renderHuman(stdout, remaining[0], payload)
	return 0
}

func execute(c *client, args []string) (json.RawMessage, error) {
	switch args[0] {
	case "meta":
		meta, err := c.serverMetadata()
		if err != nil {
			return nil, err
		}
		if meta.Service == "" {
			return nil, incompatibleServerProblem("protocol metadata", meta, "api.meta.v1")
		}
		return json.Marshal(meta)
	case "status":
		return c.request(http.MethodGet, "/api/v1/status", nil, "", "")
	case "tree", "browse":
		return executeTree(c, args[1:])
	case "read", "history", "job", "graph":
		if len(args) != 2 {
			return nil, fmt.Errorf("%s requires one ID", args[0])
		}
		path := map[string]string{
			"read":    "/api/v1/documents/",
			"history": "/api/v1/documents/",
			"job":     "/api/v1/jobs/",
			"graph":   "/api/v1/entities/",
		}[args[0]] + url.PathEscape(args[1])
		if args[0] == "history" {
			path += "/revisions"
		}
		return c.request(http.MethodGet, path, nil, "", "")
	case "jobs", "conflicts":
		return c.request(http.MethodGet, "/api/v1/"+args[0], nil, "", "")
	case "facts", "relations":
		flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
		entity := flags.String("entity", "", "filter by entity slug")
		if err := flags.Parse(args[1:]); err != nil {
			return nil, err
		}
		path := "/api/v1/" + args[0]
		if *entity != "" {
			path += "?entity_id=" + url.QueryEscape(*entity)
		}
		return c.request(http.MethodGet, path, nil, "", "")
	case "search":
		return executeSearch(c, args[1:])
	case "context":
		return executeContext(c, args[1:])
	case "remember", "write":
		return executeRemember(c, args[1:])
	case "retry-job":
		if len(args) != 2 {
			return nil, fmt.Errorf("retry-job requires a job XID")
		}
		return c.request(http.MethodPost, "/api/v1/jobs/"+url.PathEscape(args[1])+"/retry", map[string]any{}, "", newIdempotencyKey())
	case "resolve-conflict":
		if len(args) < 3 {
			return nil, fmt.Errorf("resolve-conflict requires a conflict XID and resolution")
		}
		return c.request(http.MethodPost, "/api/v1/conflicts/"+url.PathEscape(args[1])+"/resolve",
			map[string]any{"resolution": strings.Join(args[2:], " ")}, "", newIdempotencyKey())
	case "rename-preview":
		if len(args) < 2 {
			return nil, fmt.Errorf("rename-preview requires TYPE:FROM:TO operations")
		}
		operations := make([]map[string]string, 0, len(args)-1)
		for _, raw := range args[1:] {
			parts := strings.Split(raw, ":")
			if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
				return nil, fmt.Errorf("rename operation must be TYPE:FROM:TO, got %q", raw)
			}
			operations = append(operations, map[string]string{"resource_type": parts[0], "from": parts[1], "to": parts[2]})
		}
		return c.request(http.MethodPost, "/api/v1/rename-plans", map[string]any{"operations": operations}, "", newIdempotencyKey())
	case "rename-apply":
		if len(args) != 2 {
			return nil, fmt.Errorf("rename-apply requires a plan XID")
		}
		return c.request(http.MethodPost, "/api/v1/rename-plans/"+url.PathEscape(args[1])+"/apply",
			map[string]any{}, "", newIdempotencyKey())
	default:
		return nil, fmt.Errorf("unknown command %q", args[0])
	}
}

func executeTree(c *client, args []string) (json.RawMessage, error) {
	flags := flag.NewFlagSet("tree", flag.ContinueOnError)
	glob := flags.String("glob", "**", "slash-aware path glob")
	types := flags.String("types", "folder,document", "folder, document, or both")
	limit := flags.Int("limit", 200, "maximum results")
	cursor := flags.String("cursor", "", "pagination cursor")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	if *glob == "**" && *types == "folder,document" && *limit == 200 && *cursor == "" {
		return c.request(http.MethodGet, "/api/v1/tree", nil, "", "")
	}
	if err := c.requireFeatures("filtered tree", featureTreeGlob); err != nil {
		return nil, err
	}
	query := url.Values{}
	query.Set("glob", *glob)
	query.Set("types", *types)
	query.Set("limit", strconv.Itoa(*limit))
	if *cursor != "" {
		query.Set("cursor", *cursor)
	}
	return c.request(http.MethodGet, "/api/v1/tree?"+query.Encode(), nil, "", "")
}

func executeSearch(c *client, args []string) (json.RawMessage, error) {
	flags := flag.NewFlagSet("search", flag.ContinueOnError)
	mode := flags.String("mode", "hybrid", "hybrid, semantic, lexical, or graph")
	limit := flags.Int("limit", 10, "maximum results")
	history := flags.Bool("history", false, "include historical knowledge")
	scopeGlob := flags.String("scope-glob", "", "filter canonical paths with a slash-aware glob")
	sourceTypes := stringList{}
	flags.Var(&sourceTypes, "type", "document or fact; repeatable")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	query := strings.Join(flags.Args(), " ")
	if query == "" {
		return nil, fmt.Errorf("search requires a query")
	}
	required := retrievalFeatures(*scopeGlob, sourceTypes)
	if err := c.requireFeatures("scoped search", required...); err != nil {
		return nil, err
	}
	body := map[string]any{"query": query, "mode": *mode, "limit": *limit, "include_history": *history}
	addRetrievalSelectors(body, *scopeGlob, sourceTypes)
	return c.request(http.MethodPost, "/api/v1/search", body, "", "")
}

func executeContext(c *client, args []string) (json.RawMessage, error) {
	flags := flag.NewFlagSet("context", flag.ContinueOnError)
	budget := flags.Int("token-budget", 4000, "maximum estimated tokens")
	history := flags.Bool("history", false, "include historical knowledge")
	scopeGlob := flags.String("scope-glob", "", "filter canonical paths with a slash-aware glob")
	sourceTypes := stringList{}
	flags.Var(&sourceTypes, "type", "document or fact; repeatable")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	query := strings.Join(flags.Args(), " ")
	if query == "" {
		return nil, fmt.Errorf("context requires a query")
	}
	required := retrievalFeatures(*scopeGlob, sourceTypes)
	if err := c.requireFeatures("scoped context", required...); err != nil {
		return nil, err
	}
	body := map[string]any{"query": query, "token_budget": *budget, "include_history": *history}
	addRetrievalSelectors(body, *scopeGlob, sourceTypes)
	return c.request(http.MethodPost, "/api/v1/context", body, "", "")
}

func executeRemember(c *client, args []string) (json.RawMessage, error) {
	flags := flag.NewFlagSet("remember", flag.ContinueOnError)
	id := flags.String("id", "", "document semantic slug")
	title := flags.String("title", "", "document title")
	content := flags.String("content", "", "UTF-8 document content")
	file := flags.String("file", "", "read UTF-8 content from a file")
	folderPath := flags.String("folder-path", "", "canonical nested folder path")
	legacyFolder := flags.String("folder", "", "deprecated alias for --folder-path")
	format := flags.String("format", "markdown", "document format")
	query := flags.String("query", "", "semantic discovery query; defaults to title and content")
	updateID := flags.String("update", "", "update this canonical document after discovery")
	createNew := flags.Bool("new", false, "create a distinct document despite related candidates")
	reason := flags.String("reason", "", "why related candidates should not be updated")
	waitTimeout := flags.Duration("wait-timeout", 2*time.Minute, "maximum time to wait for terminal indexing")
	tags := stringList{}
	flags.Var(&tags, "tag", "document tag; repeatable")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	if *folderPath != "" && *legacyFolder != "" {
		return nil, fmt.Errorf("use only --folder-path; --folder is its deprecated alias")
	}
	if *folderPath == "" {
		*folderPath = *legacyFolder
	}
	if *updateID != "" && *createNew {
		return nil, fmt.Errorf("--update and --new are mutually exclusive decisions")
	}
	if (*content == "" && *file == "") || (*content != "" && *file != "") {
		return nil, fmt.Errorf("remember requires exactly one of --content or --file")
	}
	if err := c.requireFeatures("remember", featureRemember, featureSourceTypes); err != nil {
		return nil, err
	}
	if *file != "" {
		data, err := os.ReadFile(filepath.Clean(*file))
		if err != nil {
			return nil, err
		}
		*content = string(data)
	}
	discoveryQuery := strings.TrimSpace(*query)
	if discoveryQuery == "" {
		discoveryQuery = strings.TrimSpace(*title + " " + truncateRunes(*content, 600))
	}
	candidates, err := discoverMemoryCandidates(c, discoveryQuery)
	if err != nil {
		return nil, err
	}
	if *updateID != "" {
		if *folderPath != "" {
			return nil, fmt.Errorf("--folder-path cannot move an existing document; use a rename plan for identity changes")
		}
		return updateMemory(c, *updateID, *title, *content, []string(tags), *waitTimeout)
	}
	if *id == "" || *title == "" || *folderPath == "" {
		return nil, fmt.Errorf("new memory requires --id, --title, and --folder-path")
	}
	if exact, found, exactErr := readCandidate(c, *id); exactErr != nil {
		return nil, exactErr
	} else if found {
		candidates = prependCandidate(candidates, exact)
		return nil, candidatesProblem(candidates, "The requested document slug already exists. Use --update after reading it.")
	}
	if len(candidates) > 0 && !*createNew {
		return nil, candidatesProblem(candidates,
			"Related canonical memory exists. Read the candidates, then choose --update or explicitly justify --new.")
	}
	if len(candidates) > 0 && strings.TrimSpace(*reason) == "" {
		return nil, fmt.Errorf("--new requires --reason when discovery found related canonical memory")
	}
	metadata := map[string]any{}
	if strings.TrimSpace(*reason) != "" {
		metadata["creation_reason"] = strings.TrimSpace(*reason)
		metadata["discovery_query"] = discoveryQuery
	}
	created, err := c.request(http.MethodPost, "/api/v1/documents", map[string]any{
		"id": *id, "folder_path": *folderPath, "title": *title, "format": *format,
		"content": *content, "tags": []string(tags), "metadata": metadata,
	}, "", newIdempotencyKey())
	if err != nil {
		return nil, err
	}
	return waitForMutation(c, created, *waitTimeout)
}

func discoverMemoryCandidates(c *client, query string) ([]memoryCandidate, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("remember could not derive a discovery query; supply --query")
	}
	payload, err := c.request(http.MethodPost, "/api/v1/search", map[string]any{
		"query": query, "mode": "hybrid", "limit": 8, "source_types": []string{"document"},
	}, "", "")
	if err != nil {
		return nil, err
	}
	var response struct {
		Items                []memoryCandidate `json:"items"`
		DegradedDependencies []string          `json:"degraded_dependencies"`
	}
	if err := json.Unmarshal(payload, &response); err != nil {
		return nil, fmt.Errorf("decode discovery results: %w", err)
	}
	if len(response.DegradedDependencies) > 0 {
		var p problem
		p.Code = "discovery_degraded"
		p.Title = "Semantic memory discovery is incomplete"
		p.Detail = "Manifold could not query every retrieval dependency, so a new document could duplicate canonical memory."
		p.RequestID = "local"
		p.DegradedDependencies = response.DegradedDependencies
		p.Remediation.Summary = "Restore complete search before making a memory mutation."
		p.Remediation.Steps = []string{"Run manifold status.", "Restore the listed dependency.", "Run remember again after search is no longer degraded."}
		return nil, &apiError{Status: http.StatusServiceUnavailable, Body: p}
	}
	return response.Items, nil
}

func readCandidate(c *client, id string) (memoryCandidate, bool, error) {
	payload, err := c.request(http.MethodGet, "/api/v1/documents/"+url.PathEscape(id), nil, "", "")
	if err != nil {
		var semantic *apiError
		if errors.As(err, &semantic) && semantic.Body.Code == "resource_not_found" {
			return memoryCandidate{}, false, nil
		}
		return memoryCandidate{}, false, err
	}
	var document struct {
		ID       string `json:"id"`
		Title    string `json:"title"`
		Revision string `json:"revision"`
	}
	if err := json.Unmarshal(payload, &document); err != nil {
		return memoryCandidate{}, false, fmt.Errorf("decode canonical document: %w", err)
	}
	return memoryCandidate{
		ID: document.ID, Title: document.Title, Revision: document.Revision,
		CanonicalRef: "manifold://documents/" + document.ID + "@" + document.Revision,
	}, true, nil
}

func prependCandidate(candidates []memoryCandidate, candidate memoryCandidate) []memoryCandidate {
	result := []memoryCandidate{candidate}
	for _, existing := range candidates {
		if existing.ID != candidate.ID {
			result = append(result, existing)
		}
	}
	return result
}

func candidatesProblem(candidates []memoryCandidate, detail string) error {
	var p problem
	p.Code = "memory_candidates_found"
	p.Title = "Related canonical memory requires a decision"
	p.Detail = detail
	p.RequestID = "local"
	p.Candidates = candidates
	p.Remediation.Summary = "Read the candidates before deciding whether to update one or create a distinct document."
	p.Remediation.Steps = []string{
		"Run manifold read <candidate-id> for the relevant results.",
		"Use remember --update <candidate-id> to revise canonical knowledge.",
		"Use remember --new --reason <why-separate> only when the new knowledge has a distinct lifecycle or meaning.",
	}
	return &apiError{Status: http.StatusConflict, Body: p}
}

func updateMemory(c *client, id, title, content string, tags []string, waitTimeout time.Duration) (json.RawMessage, error) {
	path := "/api/v1/documents/" + url.PathEscape(id)
	current, headers, err := c.requestWithHeaders(http.MethodGet, path, nil, "", "")
	if err != nil {
		return nil, err
	}
	var document struct {
		Title string `json:"title"`
		ETag  string `json:"etag"`
	}
	if err := json.Unmarshal(current, &document); err != nil {
		return nil, fmt.Errorf("decode current document: %w", err)
	}
	etag := headers.Get("ETag")
	if etag == "" {
		etag = document.ETag
	}
	if etag == "" {
		return nil, fmt.Errorf("current document did not include an ETag")
	}
	if title == "" {
		title = document.Title
	}
	body := map[string]any{"title": title, "content": content}
	if tags != nil {
		body["tags"] = tags
	}
	updated, err := c.request(http.MethodPut, path, body, etag, newIdempotencyKey())
	if err != nil {
		return nil, err
	}
	return waitForMutation(c, updated, waitTimeout)
}

func waitForMutation(c *client, payload json.RawMessage, waitTimeout time.Duration) (json.RawMessage, error) {
	var response map[string]json.RawMessage
	if err := json.Unmarshal(payload, &response); err != nil {
		return nil, fmt.Errorf("decode mutation response: %w", err)
	}
	var accepted jobState
	if err := json.Unmarshal(response["job"], &accepted); err != nil || accepted.ID == "" {
		return nil, fmt.Errorf("mutation response did not include a job XID")
	}
	terminal, raw, err := waitForJob(c, accepted.ID, waitTimeout)
	if err != nil {
		return nil, err
	}
	response["job"] = raw
	if terminal.Status != "ready" {
		return nil, fmt.Errorf("job %s ended in unexpected state %s", terminal.ID, terminal.Status)
	}
	var document struct {
		ID string `json:"id"`
	}
	if rawDocument, ok := response["document"]; ok && json.Unmarshal(rawDocument, &document) == nil && document.ID != "" {
		current, err := c.request(http.MethodGet, "/api/v1/documents/"+url.PathEscape(document.ID), nil, "", "")
		if err != nil {
			return nil, err
		}
		response["document"] = current
	}
	return json.Marshal(response)
}

func waitForJob(c *client, id string, waitTimeout time.Duration) (jobState, json.RawMessage, error) {
	deadline := time.Now().Add(waitTimeout)
	for {
		payload, err := c.request(http.MethodGet, "/api/v1/jobs/"+url.PathEscape(id), nil, "", "")
		if err != nil {
			return jobState{}, nil, err
		}
		var job jobState
		if err := json.Unmarshal(payload, &job); err != nil {
			return jobState{}, nil, fmt.Errorf("decode job status: %w", err)
		}
		switch job.Status {
		case "ready":
			return job, payload, nil
		case "failed", "partially_ready":
			if job.Error != nil {
				return job, payload, &apiError{Status: http.StatusConflict, Body: *job.Error}
			}
			var p problem
			p.Code = "job_failed"
			p.Title = "Memory was not fully indexed"
			p.Detail = fmt.Sprintf("Job %s reached %s without structured remediation.", job.ID, job.Status)
			p.RequestID = "local"
			p.Remediation.Summary = "Inspect the job and server logs before retrying."
			p.Remediation.Steps = []string{"Run manifold job " + job.ID + ".", "Do not report the memory as durable until the job reaches ready."}
			return job, payload, &apiError{Status: http.StatusConflict, Body: p}
		case "accepted", "indexing", "extracting":
			if time.Now().After(deadline) {
				var p problem
				p.Code = "job_wait_timeout"
				p.Title = "Memory indexing is still running"
				p.Detail = fmt.Sprintf("Job %s did not reach a terminal state within %s.", job.ID, waitTimeout)
				p.RequestID = "local"
				p.RetryAfter = "5s"
				p.Remediation.Summary = "Inspect the durable job before deciding whether the write succeeded."
				p.Remediation.Steps = []string{"Run manifold job " + job.ID + ".", "Wait for ready, partially_ready, or failed before taking another mutation action."}
				return job, payload, &apiError{Status: http.StatusRequestTimeout, Body: p}
			}
			time.Sleep(jobPollInterval)
		default:
			return jobState{}, nil, fmt.Errorf("job %s returned unknown status %q", id, job.Status)
		}
	}
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

func retrievalFeatures(scopeGlob string, sourceTypes []string) []string {
	var required []string
	if scopeGlob != "" {
		required = append(required, featureScopeGlob)
	}
	if len(sourceTypes) > 0 {
		required = append(required, featureSourceTypes)
	}
	return required
}

func addRetrievalSelectors(body map[string]any, scopeGlob string, sourceTypes []string) {
	if scopeGlob != "" {
		body["scope_glob"] = scopeGlob
	}
	if len(sourceTypes) > 0 {
		body["source_types"] = sourceTypes
	}
}

func (c *client) requireFeatures(command string, required ...string) error {
	if len(required) == 0 {
		return nil
	}
	meta, err := c.serverMetadata()
	if err != nil {
		return err
	}
	available := make(map[string]struct{}, len(meta.Features))
	for _, feature := range meta.Features {
		available[feature] = struct{}{}
	}
	missing := make([]string, 0, len(required))
	for _, feature := range required {
		if _, ok := available[feature]; !ok {
			missing = append(missing, feature)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return incompatibleServerProblem(command, meta, missing...)
}

func incompatibleServerProblem(command string, meta serverMetadata, missing ...string) error {
	serverVersion := meta.Version
	if serverVersion == "" {
		serverVersion = "legacy-or-unknown"
	}
	var p problem
	p.Code = "server_incompatible"
	p.Title = "Manifold server does not support this command"
	p.Detail = fmt.Sprintf("%s requires features not advertised by Manifold %s: %s.", command, serverVersion, strings.Join(missing, ", "))
	p.RequestID = "local"
	p.RequiredFeatures = missing
	p.ServerVersion = serverVersion
	p.Remediation.Summary = "Deploy a compatible Manifold server before retrying this operation."
	p.Remediation.Steps = []string{
		"Ask the operator to deploy Manifold v0.2.0 or newer.",
		"Run manifold meta and confirm the required features are advertised.",
		"For read-only retrieval, use an unscoped search only if removing the filter preserves the task intent.",
	}
	return &apiError{Status: http.StatusConflict, Body: p}
}

func (c *client) serverMetadata() (serverMetadata, error) {
	if c.metaRead {
		return c.meta, nil
	}
	payload, err := c.request(http.MethodGet, "/api/v1/meta", nil, "", "")
	if err != nil {
		var semantic *apiError
		if errors.As(err, &semantic) && semantic.Status == http.StatusNotFound {
			c.metaRead = true
			return serverMetadata{}, nil
		}
		return serverMetadata{}, err
	}
	var meta serverMetadata
	if json.Unmarshal(payload, &meta) != nil || meta.Service != "manifold" || meta.APIMajor != 1 {
		c.metaRead = true
		return serverMetadata{}, nil
	}
	c.meta = meta
	c.metaRead = true
	return meta, nil
}

func (c *client) request(method, path string, body any, etag, idempotencyKey string) (json.RawMessage, error) {
	payload, _, err := c.requestWithHeaders(method, path, body, etag, idempotencyKey)
	return payload, err
}

func (c *client) requestWithHeaders(method, path string, body any, etag, idempotencyKey string) (json.RawMessage, http.Header, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, nil, err
		}
		reader = bytes.NewReader(data)
	}
	request, err := http.NewRequest(method, c.baseURL+path, reader)
	if err != nil {
		return nil, nil, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+c.apiKey)
	request.Header.Set("User-Agent", "manifold-skill/"+version)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if etag != "" {
		request.Header.Set("If-Match", etag)
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, nil, &apiError{Body: localDependencyProblem(err)}
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return nil, response.Header, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var semantic problem
		if json.Unmarshal(data, &semantic) != nil || semantic.Code == "" {
			semantic = problem{
				Title: "The server returned a non-semantic error", Code: "invalid_server_response",
				Detail: "No safe structured detail was available.", RequestID: response.Header.Get("X-Request-ID"),
			}
			semantic.Remediation.Summary = "Ask the Manifold operator to inspect the response."
		}
		return nil, response.Header, &apiError{Status: response.StatusCode, Body: semantic}
	}
	return data, response.Header, nil
}

func localDependencyProblem(err error) problem {
	var result problem
	result.Code = "dependency_unavailable"
	result.Title = "Manifold is unreachable"
	result.Detail = err.Error()
	result.RequestID = "local"
	result.Remediation.Summary = "Check MANIFOLD_URL, network access, and the remote instance readiness."
	result.Remediation.Steps = []string{"Retry status after connectivity is restored."}
	return result
}

func newIdempotencyKey() string {
	data := make([]byte, 18)
	if _, err := rand.Read(data); err != nil {
		return "skill-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return "skill-" + base64.RawURLEncoding.EncodeToString(data)
}

func renderProblem(out io.Writer, p problem) {
	fmt.Fprintf(out, "ERROR [%s] %s\n", p.Code, p.Title)
	if p.Detail != "" {
		fmt.Fprintln(out, p.Detail)
	}
	if p.Remediation.Summary != "" {
		fmt.Fprintln(out, "How to fix:", p.Remediation.Summary)
	}
	for index, step := range p.Remediation.Steps {
		fmt.Fprintf(out, "  %d. %s\n", index+1, step)
	}
	for _, violation := range p.Violations {
		fmt.Fprintf(out, "  %s: %s\n", violation.Pointer, violation.Message)
	}
	if p.SuggestedSlug != "" {
		fmt.Fprintln(out, "Suggested slug:", p.SuggestedSlug)
	}
	for _, blocker := range p.Blockers {
		fmt.Fprintf(out, "Blocker: %s at %s — %s\n", blocker.Resource, blocker.Location, blocker.Reason)
	}
	for _, candidate := range p.Candidates {
		fmt.Fprintf(out, "Candidate: %s@%s", candidate.ID, candidate.Revision)
		if candidate.Path != "" {
			fmt.Fprintf(out, " at %s", candidate.Path)
		}
		if candidate.Title != "" {
			fmt.Fprintf(out, " — %s", candidate.Title)
		}
		fmt.Fprintln(out)
		if candidate.Snippet != "" {
			fmt.Fprintln(out, "  "+candidate.Snippet)
		}
	}
	if len(p.DegradedDependencies) > 0 {
		fmt.Fprintln(out, "Unavailable discovery dependencies:", strings.Join(p.DegradedDependencies, ", "))
	}
	if len(p.RequiredFeatures) > 0 {
		fmt.Fprintln(out, "Required server features:", strings.Join(p.RequiredFeatures, ", "))
	}
	if p.ServerVersion != "" {
		fmt.Fprintln(out, "Server version:", p.ServerVersion)
	}
	if p.RetryAfter != "" {
		fmt.Fprintln(out, "Retry after:", p.RetryAfter)
	}
	if p.RequestID != "" {
		fmt.Fprintln(out, "Request ID:", p.RequestID)
	}
}

func renderHuman(out io.Writer, command string, payload json.RawMessage) {
	if command == "read" {
		var document struct {
			ID, Title, Content, Revision string
		}
		if json.Unmarshal(payload, &document) == nil {
			fmt.Fprintf(out, "# %s\n\n%s\n\n[%s@%s]\n", document.Title, document.Content, document.ID, document.Revision)
			return
		}
	}
	if command == "context" {
		var response struct {
			Context struct {
				Text string `json:"text"`
			} `json:"context"`
		}
		if json.Unmarshal(payload, &response) == nil && response.Context.Text != "" {
			fmt.Fprint(out, response.Context.Text)
			return
		}
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, payload, "", "  ") == nil {
		fmt.Fprintln(out, pretty.String())
	} else {
		fmt.Fprintln(out, string(payload))
	}
}

func exitCode(code string) int {
	switch code {
	case "validation_failed", "invalid_request":
		return 10
	case "invalid_api_key":
		return 11
	case "missing_capability", "csrf_failed":
		return 12
	case "resource_not_found":
		return 13
	case "slug_taken", "folder_path_conflict":
		return 14
	case "etag_mismatch", "idempotency_key_reused", "state_conflict", "invalid_state_transition":
		return 15
	case "stale_rename_plan", "unrewritable_reference":
		return 16
	case "dependency_unavailable", "storage_unavailable", "discovery_degraded":
		return 17
	case "job_failed":
		return 18
	case "api_key_secret_not_replayable":
		return 20
	case "memory_candidates_found":
		return 21
	case "job_wait_timeout":
		return 22
	case "server_incompatible":
		return 23
	default:
		return 19
	}
}

func printUsage(out io.Writer) {
	fmt.Fprintln(out, "Usage: manifold [--json] [--timeout 30s] COMMAND [ARGS]")
	fmt.Fprintln(out, "Commands: meta status tree browse read remember write search context history jobs job retry-job graph facts relations conflicts resolve-conflict rename-preview rename-apply")
}

type stringList []string

func (values *stringList) String() string { return strings.Join(*values, ",") }
func (values *stringList) Set(value string) error {
	*values = append(*values, value)
	return nil
}
