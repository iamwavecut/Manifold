package upstream

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/iamwavecut/Manifold/internal/model"
)

type Brain struct {
	http       httpClient
	ingestHTTP httpClient
}

type brainFactProfile struct {
	Status     string
	ValidUntil *time.Time
}

const brainSearchMaxLimit = 100

var (
	manifoldOriginPattern = regexp.MustCompile(`^manifold://documents/([a-z0-9]+(?:-[a-z0-9]+)*)@(r[1-9][0-9]*)$`)
	brainRetryKeyPattern  = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
)

func NewBrain(base, key string, client, ingestClient *http.Client) *Brain {
	auth := func(req *http.Request, key string) { req.Header.Set("Authorization", "Bearer "+key) }
	return &Brain{
		http:       httpClient{name: "brain", base: base, key: key, client: client, auth: auth},
		ingestHTTP: httpClient{name: "brain", base: base, key: key, client: ingestClient, auth: auth},
	}
}

func (b *Brain) Health(ctx context.Context) error {
	if b.http.base == "" {
		return &DependencyError{Dependency: "brain", Operation: "GET /health", Retryable: true, Cause: fmt.Errorf("BRAIN_URL is not configured")}
	}
	return b.http.do(ctx, http.MethodGet, "/health", nil, nil)
}

func (b *Brain) IngestDocument(ctx context.Context, document GraphDocument) (GraphIngestResult, error) {
	documentID, err := b.ingestDocument(ctx, document, "sync", false)
	if err != nil {
		return GraphIngestResult{}, err
	}
	state, err := b.DocumentResult(ctx, documentID)
	if err != nil {
		return GraphIngestResult{}, err
	}
	switch state.Status {
	case GraphDocumentReady:
		return state.Result, nil
	case GraphDocumentPartial, GraphDocumentFailed:
		return state.Result, &DependencyError{
			Dependency: "brain", Operation: "POST /v1/ingest/document", Retryable: false,
			Cause: fmt.Errorf("document extraction ended with status %s", state.Status),
		}
	default:
		return state.Result, &DependencyError{
			Dependency: "brain", Operation: "POST /v1/ingest/document", Retryable: true,
			Cause: fmt.Errorf("document extraction is still pending"),
		}
	}
}

// SubmitDocument stores a per-Manifold-revision source identity and asks
// Brain to extract it asynchronously. The returned ID is the durable
// document checkpoint, not a transient queue operation ID.
func (b *Brain) SubmitDocument(ctx context.Context, document GraphDocument) (string, error) {
	return b.ingestDocument(ctx, document, "async", true)
}

func (b *Brain) ingestDocument(ctx context.Context, document GraphDocument, mode string, storeContent bool) (string, error) {
	originURI, err := canonicalManifoldOrigin(document.ID, document.Revision)
	if err != nil {
		return "", err
	}
	if document.OccurredAt == "" {
		document.OccurredAt = time.Now().UTC().Format(time.RFC3339)
	}
	body := map[string]any{
		"kind":         document.Format,
		"text":         document.Content,
		"originUri":    originURI,
		"title":        document.Title,
		"occurredAt":   document.OccurredAt,
		"contextRef":   map[string]string{"vertical": "manifold", "recorder": "manifold"},
		"storeContent": storeContent,
		"indexers":     "general",
		"mode":         mode,
		"meta":         map[string]string{"manifold_id": document.ID, "revision": document.Revision},
	}
	var response struct {
		DocumentID string `json:"documentId"`
	}
	var submitErr error
	if mode == "async" {
		submitErr = b.ingestHTTP.doWithRateLimitRetry(ctx, http.MethodPost, "/v1/ingest/document", body, &response)
	} else {
		submitErr = b.ingestHTTP.do(ctx, http.MethodPost, "/v1/ingest/document", body, &response)
	}
	if submitErr != nil {
		return "", submitErr
	}
	if response.DocumentID == "" {
		return "", &DependencyError{
			Dependency: "brain", Operation: "POST /v1/ingest/document", Status: http.StatusBadGateway,
			Retryable: true, Cause: fmt.Errorf("upstream response omitted document ID"),
		}
	}
	return response.DocumentID, nil
}

func canonicalManifoldOrigin(documentID, revision string) (string, error) {
	origin := "manifold://documents/" + documentID + "@" + revision
	if !manifoldOriginPattern.MatchString(origin) {
		return "", fmt.Errorf("invalid Manifold document revision identity")
	}
	return origin, nil
}

// DocumentResult reads Brain's run ledger and committed candidate graph.
// Run states, rather than the document header's status, decide completion:
// v2.2 may leave the header at indexing after all runs and candidates settle.
func (b *Brain) DocumentResult(ctx context.Context, documentID string) (GraphDocumentState, error) {
	path := "/v1/documents/" + url.PathEscape(documentID)
	var document brainDocument
	if err := b.http.doWithRateLimitRetry(ctx, http.MethodGet, path, nil, &document); err != nil {
		return GraphDocumentState{}, err
	}
	if document.ID != "" && document.ID != documentID {
		return GraphDocumentState{}, &DependencyError{
			Dependency: "brain", Operation: "GET /v1/documents/{id}", Status: http.StatusBadGateway,
			Cause: fmt.Errorf("upstream returned a different document"),
		}
	}
	originURI, revision, err := documentIdentity(document)
	if err != nil {
		return GraphDocumentState{}, err
	}
	state := GraphDocumentState{
		BrainStatus: document.Status,
		Result: GraphIngestResult{
			DocumentID: documentID,
			OriginURI:  originURI,
			Revision:   revision,
			Raw:        map[string]any{"brain_status": document.Status},
		},
	}
	activeRuns := false
	failedRuns := 0
	for _, run := range document.Runs {
		state.Runs = append(state.Runs, GraphIndexerRun{
			IndexerID: run.PackID,
			Status:    run.Status,
			Reason:    run.Error.Message,
		})
		switch run.Status {
		case "pending", "running":
			activeRuns = true
		case "failed":
			failedRuns++
		case "succeeded", "skipped":
		default:
			// Unknown states are not evidence of completion.
			activeRuns = true
		}
	}
	if len(document.Runs) == 0 && document.Status == "failed" {
		failedRuns = 1
	}
	if len(document.Runs) == 0 && document.Status != "committed" && document.Status != "indexed" && document.Status != "failed" && document.Status != "purged" {
		activeRuns = true
	}
	if activeRuns {
		state.Status = GraphDocumentPending
		return state, nil
	}

	var candidateResponse brainCandidateList
	candidatePath := path + "/candidates"
	if err := b.http.doWithRateLimitRetry(ctx, http.MethodGet, candidatePath, nil, &candidateResponse); err != nil {
		return GraphDocumentState{}, err
	}
	if candidateResponse.DocumentID != "" && candidateResponse.DocumentID != documentID {
		return GraphDocumentState{}, &DependencyError{
			Dependency: "brain", Operation: "GET /v1/documents/{id}/candidates", Status: http.StatusBadGateway,
			Cause: fmt.Errorf("upstream returned candidates for a different document"),
		}
	}
	for _, candidate := range candidateResponse.Candidates {
		if candidate.Status == "pending" {
			state.Status = GraphDocumentPending
			return state, nil
		}
	}

	result, err := b.materializeCandidates(ctx, documentID, originURI, revision, candidateResponse.Candidates)
	if err != nil {
		return GraphDocumentState{}, err
	}
	state.Result = result
	switch {
	case failedRuns > 0 && graphResultHasData(result):
		state.Status = GraphDocumentPartial
	case failedRuns > 0:
		state.Status = GraphDocumentFailed
	default:
		state.Status = GraphDocumentReady
	}
	return state, nil
}

func documentIdentity(document brainDocument) (string, string, error) {
	originURI := document.OriginURI
	if originURI == "" {
		manifoldID := stringValue(document.Meta, "manifold_id")
		revision := stringValue(document.Meta, "revision")
		var err error
		originURI, err = canonicalManifoldOrigin(manifoldID, revision)
		if err != nil {
			return "", "", err
		}
	}
	match := manifoldOriginPattern.FindStringSubmatch(originURI)
	if len(match) != 3 {
		return "", "", fmt.Errorf("Brain document lacks a canonical Manifold source revision")
	}
	if manifoldID := stringValue(document.Meta, "manifold_id"); manifoldID != "" && manifoldID != match[1] {
		return "", "", fmt.Errorf("Brain source identity does not match document metadata")
	}
	if revision := stringValue(document.Meta, "revision"); revision != "" && revision != match[2] {
		return "", "", fmt.Errorf("Brain source revision does not match document metadata")
	}
	return originURI, match[2], nil
}

func (b *Brain) materializeCandidates(
	ctx context.Context,
	documentID, originURI, revision string,
	candidates []brainCandidate,
) (GraphIngestResult, error) {
	result := GraphIngestResult{
		DocumentID: documentID,
		OriginURI:  originURI,
		Revision:   revision,
		Raw:        map[string]any{},
	}
	entityByIndex := make(map[string]string)
	entityByID := make(map[string]GraphEntity)
	// Brain v2.2's fact detail endpoint omits validUntil and the current
	// lifecycle status; entity profiles include both while hydrating facts.
	factProfileByID := make(map[string]brainFactProfile)
	factByID := make(map[string]GraphFact)
	relationByID := make(map[string]GraphRelation)
	relationCandidates := make([]brainCandidate, 0)

	for _, candidate := range candidates {
		if !candidateCommitted(candidate) || candidate.CommitRef == "" || candidate.Kind != "entity" {
			continue
		}
		entity, profiles, err := b.getGraphEntity(ctx, candidate.CommitRef)
		if err != nil {
			return GraphIngestResult{}, err
		}
		entityByID[entity.BrainID] = entity
		for factID, profile := range profiles {
			factProfileByID[factID] = profile
		}
		if index, ok := payloadIndex(candidate.Payload, "entityIndex"); ok {
			entityByIndex[candidateBatchKey(candidate, index)] = entity.BrainID
		}
	}

	// Candidates are ordered by chunk and creation time, not by kind. Resolve
	// all entity indexes before reading fact and relation candidates.
	for _, candidate := range candidates {
		if !candidateCommitted(candidate) || candidate.CommitRef == "" {
			continue
		}
		switch candidate.Kind {
		case "fact":
			if candidate.Payload["redacted"] == true {
				continue
			}
			index, ok := payloadIndex(candidate.Payload, "entityIndex")
			if !ok {
				continue
			}
			entityID := entityByIndex[candidateBatchKey(candidate, index)]
			if entityID == "" {
				continue
			}
			fact, err := b.getGraphFact(ctx, candidate.CommitRef, entityID, documentID)
			if err != nil {
				return GraphIngestResult{}, err
			}
			profile := factProfileByID[fact.BrainID]
			if fact.Status != "retracted" && profile.Status != "" {
				fact.Status = profile.Status
			}
			fact.ValidUntil = profile.ValidUntil
			factByID[fact.BrainID] = fact
		case "relation":
			relationCandidates = append(relationCandidates, candidate)
		}
	}

	edgesByEntity := make(map[string]map[string]brainConnection)
	for _, candidate := range relationCandidates {
		fromIndex, fromOK := payloadIndex(candidate.Payload, "fromEntityIndex")
		toIndex, toOK := payloadIndex(candidate.Payload, "toEntityIndex")
		if !fromOK || !toOK {
			continue
		}
		fromID := entityByIndex[candidateBatchKey(candidate, fromIndex)]
		toID := entityByIndex[candidateBatchKey(candidate, toIndex)]
		if fromID == "" || toID == "" {
			continue
		}
		for _, entityID := range uniqueStrings(fromID, toID) {
			if _, ok := edgesByEntity[entityID]; ok {
				continue
			}
			edges, err := b.entityConnections(ctx, entityID)
			if err != nil {
				return GraphIngestResult{}, err
			}
			edgesByEntity[entityID] = edges
		}
		for _, edge := range edgesByEntity[fromID] {
			// Brain reuses an existing edge when another document confirms
			// the same relation. The committed candidate binds this source
			// revision; the edge's original source remains the first document.
			if edge.EdgeID != candidate.CommitRef || edge.From != fromID || edge.To != toID {
				continue
			}
			relationByID[edge.EdgeID] = GraphRelation{
				BrainID:           edge.EdgeID,
				FromEntityBrainID: edge.From,
				ToEntityBrainID:   edge.To,
				Predicate:         edge.Kind,
				Status:            "active",
				Confidence:        edge.Weight,
				SourceDocumentID:  documentID,
			}
			break
		}
	}
	for _, relation := range relationByID {
		result.Relations = append(result.Relations, relation)
	}

	for _, entity := range entityByID {
		result.Entities = append(result.Entities, entity)
		result.EntityIDs = append(result.EntityIDs, entity.BrainID)
	}
	for _, fact := range factByID {
		result.Facts = append(result.Facts, fact)
		result.FactIDs = append(result.FactIDs, fact.BrainID)
	}
	for _, relation := range result.Relations {
		result.EdgeIDs = append(result.EdgeIDs, relation.BrainID)
	}
	sort.Slice(result.Entities, func(i, j int) bool { return result.Entities[i].BrainID < result.Entities[j].BrainID })
	sort.Slice(result.Facts, func(i, j int) bool { return result.Facts[i].BrainID < result.Facts[j].BrainID })
	sort.Slice(result.Relations, func(i, j int) bool { return result.Relations[i].BrainID < result.Relations[j].BrainID })
	sort.Strings(result.EntityIDs)
	sort.Strings(result.FactIDs)
	sort.Strings(result.EdgeIDs)
	return result, nil
}

func (b *Brain) getGraphEntity(ctx context.Context, entityID string) (GraphEntity, map[string]brainFactProfile, error) {
	var response struct {
		EntityID      string `json:"entityId"`
		Type          string `json:"type"`
		CanonicalName string `json:"canonicalName"`
		Facts         []struct {
			FactID     string `json:"factId"`
			Status     string `json:"status"`
			ValidUntil string `json:"validUntil"`
		} `json:"facts"`
	}
	path := "/v1/entities/" + url.PathEscape(entityID)
	if err := b.http.doWithRateLimitRetry(ctx, http.MethodGet, path, nil, &response); err != nil {
		return GraphEntity{}, nil, err
	}
	if response.EntityID == "" || response.CanonicalName == "" {
		return GraphEntity{}, nil, &DependencyError{
			Dependency: "brain", Operation: "GET /v1/entities/{id}", Status: http.StatusBadGateway,
			Cause: fmt.Errorf("upstream entity response was incomplete"),
		}
	}
	factProfileByID := make(map[string]brainFactProfile, len(response.Facts))
	for _, fact := range response.Facts {
		if fact.FactID == "" {
			continue
		}
		profile := brainFactProfile{Status: fact.Status}
		if fact.ValidUntil != "" {
			validUntil, err := time.Parse(time.RFC3339Nano, fact.ValidUntil)
			if err != nil {
				return GraphEntity{}, nil, &DependencyError{
					Dependency: "brain", Operation: "GET /v1/entities/{id}", Status: http.StatusBadGateway,
					Cause: fmt.Errorf("upstream entity fact validity timestamp was invalid"),
				}
			}
			profile.ValidUntil = &validUntil
		}
		factProfileByID[fact.FactID] = profile
	}
	return GraphEntity{BrainID: response.EntityID, Name: response.CanonicalName, Type: response.Type}, factProfileByID, nil
}

func (b *Brain) getGraphFact(ctx context.Context, factID, entityID, documentID string) (GraphFact, error) {
	var response struct {
		FactID     string  `json:"factId"`
		Aspect     string  `json:"aspect"`
		Statement  string  `json:"statement"`
		Confidence float64 `json:"confidence"`
		ValidFrom  string  `json:"validFrom"`
		Retracted  bool    `json:"retracted"`
	}
	path := "/v1/facts/" + url.PathEscape(factID)
	if err := b.http.doWithRateLimitRetry(ctx, http.MethodGet, path, nil, &response); err != nil {
		return GraphFact{}, err
	}
	if response.FactID == "" || response.Aspect == "" {
		return GraphFact{}, &DependencyError{
			Dependency: "brain", Operation: "GET /v1/facts/{id}", Status: http.StatusBadGateway,
			Cause: fmt.Errorf("upstream fact response was incomplete"),
		}
	}
	validFrom, err := time.Parse(time.RFC3339Nano, response.ValidFrom)
	if err != nil {
		return GraphFact{}, &DependencyError{
			Dependency: "brain", Operation: "GET /v1/facts/{id}", Status: http.StatusBadGateway,
			Cause: fmt.Errorf("upstream fact validity timestamp was invalid"),
		}
	}
	status := "active"
	if response.Retracted {
		status = "retracted"
	}
	return GraphFact{
		BrainID:          response.FactID,
		EntityBrainID:    entityID,
		Predicate:        response.Aspect,
		Object:           response.Statement,
		Status:           status,
		Confidence:       response.Confidence,
		ValidFrom:        validFrom,
		SourceDocumentID: documentID,
	}, nil
}

func (b *Brain) entityConnections(ctx context.Context, entityID string) (map[string]brainConnection, error) {
	var response struct {
		Edges []brainConnection `json:"edges"`
	}
	path := "/v1/entities/" + url.PathEscape(entityID) + "/connections"
	if err := b.http.doWithRateLimitRetry(ctx, http.MethodGet, path, nil, &response); err != nil {
		return nil, err
	}
	edges := make(map[string]brainConnection, len(response.Edges))
	for _, edge := range response.Edges {
		if edge.EdgeID != "" {
			edges[edge.EdgeID] = edge
		}
	}
	return edges, nil
}

func candidateCommitted(candidate brainCandidate) bool {
	switch candidate.Status {
	case "committed", "merged", "duplicate":
		return true
	default:
		return false
	}
}

func payloadIndex(payload map[string]any, key string) (int, bool) {
	value, ok := payload[key]
	if !ok {
		return 0, false
	}
	switch number := value.(type) {
	case float64:
		if number < 0 || number != float64(int(number)) {
			return 0, false
		}
		return int(number), true
	case int:
		return number, number >= 0
	case jsonNumber:
		parsed, err := strconv.Atoi(string(number))
		return parsed, err == nil && parsed >= 0
	default:
		return 0, false
	}
}

type jsonNumber string

func candidateBatchKey(candidate brainCandidate, index int) string {
	return candidate.RunID + "/" + strconv.Itoa(candidate.ChunkSeq) + "/" + strconv.Itoa(index)
}

func uniqueStrings(values ...string) []string {
	seen := make(map[string]struct{}, len(values))
	unique := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; value == "" || ok {
			continue
		}
		seen[value] = struct{}{}
		unique = append(unique, value)
	}
	return unique
}

func graphResultHasData(result GraphIngestResult) bool {
	return len(result.Entities) > 0 || len(result.Facts) > 0 || len(result.Relations) > 0
}

func (b *Brain) RetryDocument(ctx context.Context, documentID, retryKey string) error {
	if documentID == "" || !brainRetryKeyPattern.MatchString(retryKey) {
		return fmt.Errorf("invalid Brain document retry request")
	}
	path := "/v1/documents/" + url.PathEscape(documentID) + "/retry"
	return b.http.do(ctx, http.MethodPost, path, map[string]string{"retryKey": retryKey}, nil)
}

func (b *Brain) Search(ctx context.Context, query string, limit int, includeHistory bool) ([]model.SearchHit, error) {
	body := map[string]any{
		"query": query, "limit": min(limit, brainSearchMaxLimit), "searchMode": "hybrid",
		"requireProvenance": true, "includeContested": true,
		"includeStale": includeHistory,
	}
	var response struct {
		Results []struct {
			EntityID      string  `json:"entityId"`
			EntityType    string  `json:"entityType"`
			CanonicalName string  `json:"canonicalName"`
			Score         float64 `json:"score"`
			Facts         []struct {
				FactID    string  `json:"factId"`
				Predicate string  `json:"predicate"`
				Object    string  `json:"object"`
				Score     float64 `json:"score"`
				SourceKey string  `json:"sourceKey"`
			} `json:"facts"`
		} `json:"results"`
	}
	if err := b.http.do(ctx, http.MethodPost, "/v1/search", body, &response); err != nil {
		return nil, err
	}
	var hits []model.SearchHit
	for _, entity := range response.Results {
		for _, fact := range entity.Facts {
			if fact.FactID == "" {
				continue
			}
			hits = append(hits, model.SearchHit{
				Kind: "fact", ID: fact.FactID, Title: entity.CanonicalName,
				Snippet: fmt.Sprintf("%s %s", fact.Predicate, fact.Object),
				Score:   max(entity.Score, fact.Score), Source: "brain",
			})
		}
	}
	return hits, nil
}

func (b *Brain) IngestFact(ctx context.Context, fact model.Fact, entity model.Entity) (string, string, error) {
	body := map[string]any{
		"entityRef":  map[string]string{"vertical": entity.Kind, "id": entity.ID},
		"predicate":  fact.Predicate,
		"object":     fact.Object,
		"validFrom":  fact.ValidFrom.UTC().Format(time.RFC3339),
		"confidence": fact.Confidence,
		"source": map[string]any{
			"vertical": "manifold", "recorder": "manifold",
			"evidence": []map[string]string{{"kind": "document", "ref": fact.SourceDocumentID}},
		},
		"metadata": map[string]string{"manifold_fact_id": fact.ID},
		"explain":  true,
	}
	if fact.ValidUntil != nil {
		body["validUntil"] = fact.ValidUntil.UTC().Format(time.RFC3339)
	}
	var response struct {
		FactID  string `json:"factId"`
		Outcome string `json:"outcome"`
	}
	err := b.http.do(ctx, http.MethodPost, "/v1/ingest/fact", body, &response)
	return response.FactID, response.Outcome, err
}

func (b *Brain) IngestRelation(ctx context.Context, relation model.Relation, from, to model.Entity) (string, error) {
	body := map[string]any{
		"from":   map[string]string{"vertical": from.Kind, "id": from.ID},
		"to":     map[string]string{"vertical": to.Kind, "id": to.ID},
		"kind":   relation.Predicate,
		"weight": relation.Confidence,
		"source": map[string]string{"vertical": "manifold"},
	}
	var response map[string]any
	err := b.http.do(ctx, http.MethodPost, "/v1/ingest/link", body, &response)
	return stringValue(response, "edgeId"), err
}

func (b *Brain) RetractFact(ctx context.Context, upstreamID, reason string) error {
	path := "/v1/facts/" + url.PathEscape(upstreamID) + "/retract"
	body := map[string]any{
		"reason":      reason,
		"retractedBy": map[string]string{"source": "system"},
	}
	return b.http.do(ctx, http.MethodPost, path, body, nil)
}

func (b *Brain) GetEntity(ctx context.Context, upstreamID string) (map[string]any, error) {
	var response map[string]any
	path := "/v1/entities/" + url.PathEscape(upstreamID)
	err := b.http.do(ctx, http.MethodGet, path, nil, &response)
	return response, err
}

func stringValue(raw map[string]any, key string) string {
	value, _ := raw[key].(string)
	return value
}

type brainDocument struct {
	ID        string         `json:"id"`
	Status    string         `json:"status"`
	OriginURI string         `json:"originUri"`
	Meta      map[string]any `json:"meta"`
	Runs      []struct {
		RunID  string `json:"runId"`
		PackID string `json:"packId"`
		Status string `json:"status"`
		Error  struct {
			Message string `json:"message"`
		} `json:"error"`
	} `json:"runs"`
}

type brainCandidateList struct {
	DocumentID string           `json:"documentId"`
	Candidates []brainCandidate `json:"candidates"`
}

type brainCandidate struct {
	ID        string         `json:"id"`
	RunID     string         `json:"runId"`
	ChunkSeq  int            `json:"chunkSeq"`
	Kind      string         `json:"kind"`
	Status    string         `json:"status"`
	CommitRef string         `json:"commitRef"`
	Payload   map[string]any `json:"payload"`
}

type brainConnection struct {
	EdgeID    string         `json:"edgeId"`
	From      string         `json:"from"`
	To        string         `json:"to"`
	Kind      string         `json:"kind"`
	Weight    float64        `json:"weight"`
	Source    map[string]any `json:"source"`
	CreatedAt string         `json:"createdAt"`
}
