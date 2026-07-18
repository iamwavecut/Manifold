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

type client struct {
	baseURL string
	apiKey  string
	http    *http.Client
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
}

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
		fmt.Fprintln(stdout, version)
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
	case "status":
		return c.request(http.MethodGet, "/api/v1/status", nil, "", "")
	case "tree":
		return c.request(http.MethodGet, "/api/v1/tree", nil, "", "")
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
	case "write":
		return executeWrite(c, args[1:])
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

func executeSearch(c *client, args []string) (json.RawMessage, error) {
	flags := flag.NewFlagSet("search", flag.ContinueOnError)
	mode := flags.String("mode", "hybrid", "hybrid, semantic, lexical, or graph")
	limit := flags.Int("limit", 10, "maximum results")
	history := flags.Bool("history", false, "include historical knowledge")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	query := strings.Join(flags.Args(), " ")
	if query == "" {
		return nil, fmt.Errorf("search requires a query")
	}
	return c.request(http.MethodPost, "/api/v1/search", map[string]any{
		"query": query, "mode": *mode, "limit": *limit, "include_history": *history,
	}, "", "")
}

func executeContext(c *client, args []string) (json.RawMessage, error) {
	flags := flag.NewFlagSet("context", flag.ContinueOnError)
	budget := flags.Int("token-budget", 4000, "maximum estimated tokens")
	history := flags.Bool("history", false, "include historical knowledge")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	query := strings.Join(flags.Args(), " ")
	if query == "" {
		return nil, fmt.Errorf("context requires a query")
	}
	return c.request(http.MethodPost, "/api/v1/context", map[string]any{
		"query": query, "token_budget": *budget, "include_history": *history,
	}, "", "")
}

func executeWrite(c *client, args []string) (json.RawMessage, error) {
	flags := flag.NewFlagSet("write", flag.ContinueOnError)
	id := flags.String("id", "", "document semantic slug")
	title := flags.String("title", "", "document title")
	content := flags.String("content", "", "UTF-8 document content")
	file := flags.String("file", "", "read UTF-8 content from a file")
	folder := flags.String("folder", "", "parent folder slug")
	format := flags.String("format", "markdown", "document format")
	tags := stringList{}
	flags.Var(&tags, "tag", "document tag; repeatable")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	if *id == "" || *title == "" || (*content == "" && *file == "") || (*content != "" && *file != "") {
		return nil, fmt.Errorf("write requires --id, --title, and exactly one of --content or --file")
	}
	if *file != "" {
		data, err := os.ReadFile(filepath.Clean(*file))
		if err != nil {
			return nil, err
		}
		*content = string(data)
	}
	path := "/api/v1/documents/" + url.PathEscape(*id)
	current, headers, err := c.requestWithHeaders(http.MethodGet, path, nil, "", "")
	if err != nil {
		var semantic *apiError
		if !errors.As(err, &semantic) || semantic.Body.Code != "resource_not_found" {
			return nil, err
		}
		return c.request(http.MethodPost, "/api/v1/documents", map[string]any{
			"id": *id, "folder_id": *folder, "title": *title, "format": *format, "content": *content, "tags": []string(tags),
		}, "", newIdempotencyKey())
	}
	etag := headers.Get("ETag")
	if etag == "" {
		var document struct {
			ETag string `json:"etag"`
		}
		_ = json.Unmarshal(current, &document)
		etag = document.ETag
	}
	body := map[string]any{"title": *title, "content": *content, "tags": []string(tags)}
	key := newIdempotencyKey()
	updated, err := c.request(http.MethodPut, path, body, etag, key)
	if err == nil {
		return updated, nil
	}
	var semantic *apiError
	if !errors.As(err, &semantic) || semantic.Body.Code != "etag_mismatch" {
		return nil, err
	}
	latest, latestHeaders, getErr := c.requestWithHeaders(http.MethodGet, path, nil, "", "")
	if getErr != nil {
		return nil, getErr
	}
	latestETag := latestHeaders.Get("ETag")
	if latestETag == "" {
		var document struct {
			ETag string `json:"etag"`
		}
		_ = json.Unmarshal(latest, &document)
		latestETag = document.ETag
	}
	return c.request(http.MethodPut, path, body, latestETag, key)
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
	case "slug_taken":
		return 14
	case "etag_mismatch", "idempotency_key_reused", "state_conflict", "invalid_state_transition":
		return 15
	case "stale_rename_plan", "unrewritable_reference":
		return 16
	case "dependency_unavailable", "storage_unavailable":
		return 17
	case "job_failed":
		return 18
	case "api_key_secret_not_replayable":
		return 20
	default:
		return 19
	}
}

func printUsage(out io.Writer) {
	fmt.Fprintln(out, "Usage: manifold [--json] [--timeout 30s] COMMAND [ARGS]")
	fmt.Fprintln(out, "Commands: status tree read write search context history jobs job retry-job graph facts relations conflicts resolve-conflict rename-preview rename-apply")
}

type stringList []string

func (values *stringList) String() string { return strings.Join(*values, ",") }
func (values *stringList) Set(value string) error {
	*values = append(*values, value)
	return nil
}
