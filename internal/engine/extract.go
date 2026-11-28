package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/iamwavecut/Manifold/internal/llm"
	"github.com/iamwavecut/Manifold/internal/upstream"
)

// extractionChunkSize is the largest part of a document one model call
// reads. Most documents fit in one call.
const extractionChunkSize = 12000

const (
	maxEntitiesPerPart  = 80
	maxFactsPerPart     = 120
	maxRelationsPerPart = 80
	maxValueRunes       = 1000
	maxNameRunes        = 200
	maxEvidenceRunes    = 500
	maxEntityHints      = 40
)

var entityKinds = []string{
	"project", "asset", "staff", "person", "customer", "organization", "location", "topic", "document", "other",
}

// singleValued predicates hold one current value per entity: a newer value
// supersedes the older one. Every other predicate keeps all its values.
var singleValued = map[string]bool{
	"status": true, "os_version": true, "code_memory__default_value": true,
}

// defaultPredicates seed the vocabulary of a new index.
var defaultPredicates = []string{
	"code_memory__invariant", "identifier", "code_memory__measured_performance", "code_memory__gotcha",
	"code_memory__decided", "name", "status", "code_memory__default_value", "code_memory__depends_on_version",
	"code_memory__because", "verification_result", "state_change", "intent", "instruction",
	"code_memory__superseded_by", "product_description", "duration_limit", "hardware", "reference_example",
	"code_memory__owns", "comparison", "content_guideline", "http_status", "forbidden_pattern", "os_version",
	"service_port", "runs_on", "rate_limit", "naming_prefix", "preference", "email",
}

// Extraction is the model's answer for one part of a document.
type Extraction struct {
	Entities  []ExtractedEntity   `json:"entities"`
	Facts     []ExtractedFact     `json:"facts"`
	Relations []ExtractedRelation `json:"relations"`
}

type ExtractedEntity struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	Aliases []string `json:"aliases"`
}

type ExtractedFact struct {
	Entity     flexInt   `json:"entity"`
	Predicate  string    `json:"predicate"`
	Value      string    `json:"value"`
	ValidFrom  string    `json:"valid_from"`
	Confidence flexFloat `json:"confidence"`
	Evidence   string    `json:"evidence"`
}

type ExtractedRelation struct {
	From       flexInt   `json:"from"`
	To         flexInt   `json:"to"`
	Predicate  string    `json:"predicate"`
	Confidence flexFloat `json:"confidence"`
}

// flexInt and flexFloat also accept numbers written as strings, which some
// models produce.
type flexInt int

func (v *flexInt) UnmarshalJSON(data []byte) error {
	parsed, err := strconv.Atoi(strings.Trim(string(data), `"`))
	if err != nil {
		*v = -1
		return nil
	}
	*v = flexInt(parsed)
	return nil
}

type flexFloat float64

func (v *flexFloat) UnmarshalJSON(data []byte) error {
	parsed, err := strconv.ParseFloat(strings.Trim(string(data), `"`), 64)
	if err != nil {
		*v = 0
		return nil
	}
	*v = flexFloat(parsed)
	return nil
}

var extractionSchema = func() map[string]any {
	str := map[string]any{"type": "string"}
	integer := map[string]any{"type": "integer"}
	number := map[string]any{"type": "number"}
	object := func(properties map[string]any) map[string]any {
		required := make([]string, 0, len(properties))
		for name := range properties {
			required = append(required, name)
		}
		slices.Sort(required)
		return map[string]any{
			"type": "object", "properties": properties, "required": required, "additionalProperties": false,
		}
	}
	array := func(items map[string]any) map[string]any { return map[string]any{"type": "array", "items": items} }
	return object(map[string]any{
		"entities": array(object(map[string]any{
			"name": str, "type": map[string]any{"type": "string", "enum": entityKinds}, "aliases": array(str),
		})),
		"facts": array(object(map[string]any{
			"entity": integer, "predicate": str, "value": str, "valid_from": str,
			"confidence": number, "evidence": str,
		})),
		"relations": array(object(map[string]any{
			"from": integer, "to": integer, "predicate": str, "confidence": number,
		})),
	})
}()

func extractionSystem(vocabulary []string) string {
	return `You turn one part of a document from a team knowledge base into graph records. Answer with one JSON object that matches the schema.

entities: the named things that the facts and relations are about.
- name: the most specific proper name the text uses. If a known entity is meant, copy its name exactly.
- type: one of ` + strings.Join(entityKinds, ", ") + `.
- aliases: other names the text uses for the same entity, or [].

facts: durable statements about one entity.
- entity: the index of the entity in entities.
- predicate: snake_case English. Use a known predicate when one fits; create a new one only when none fits.
- value: what the statement says, short and self-contained, in the language of the text; never a pronoun.
- valid_from: the date (YYYY-MM-DD) on which the text says the statement became true, or "".
- confidence: from 0 to 1, how clearly the text states it.
- evidence: the shortest exact quote from the text that states it.

relations: how two entities are connected.
- from, to: indexes of entities.
- predicate: snake_case English, such as uses, depends_on, part_of, runs_on, owns.

Record only what the text states. Skip greetings, open questions, undecided plans, and instructions to the reader. Leave an item out rather than guess.

Known predicates: ` + strings.Join(vocabulary, ", ")
}

type entityHint struct{ name, kind string }

func extractionInput(document upstream.GraphDocument, part section, index, total int, hints []entityHint) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "Document: %s\n", document.Title)
	if total > 1 {
		fmt.Fprintf(&builder, "Part %d of %d\n", index+1, total)
	}
	if part.Heading != "" {
		fmt.Fprintf(&builder, "Section: %s\n", part.Heading)
	}
	if len(hints) > 0 {
		builder.WriteString("Known entities:\n")
		for _, hint := range hints {
			fmt.Fprintf(&builder, "- %s (%s)\n", hint.name, hint.kind)
		}
	}
	builder.WriteString("Text:\n\"\"\"\n")
	builder.WriteString(part.Text)
	builder.WriteString("\n\"\"\"")
	return builder.String()
}

// cleanPart is one validated part with indexes into its own entities.
type cleanPart struct {
	entities  []cleanEntity
	facts     []cleanFact
	relations []cleanRelation
}

type cleanEntity struct {
	name, kind string
	aliases    []string
}

type cleanFact struct {
	entity     int
	predicate  string
	value      string
	evidence   string
	confidence float64
	validFrom  time.Time
}

type cleanRelation struct {
	from, to   int
	predicate  string
	confidence float64
}

// clean drops what the model got wrong: unknown indexes, empty or oversized
// names and values, predicates that are not snake_case, and facts whose
// words do not occur in the text.
func clean(raw Extraction, text string, occurred time.Time) cleanPart {
	var part cleanPart
	remap := make([]int, len(raw.Entities))
	for i, entity := range raw.Entities {
		remap[i] = -1
		name := strings.TrimSpace(entity.Name)
		if !isLimitedText(name, maxNameRunes) || normalize(name) == "" || len(part.entities) == maxEntitiesPerPart {
			continue
		}
		kind := strings.ToLower(strings.TrimSpace(entity.Type))
		if !slices.Contains(entityKinds, kind) {
			kind = "other"
		}
		var aliases []string
		for _, alias := range entity.Aliases {
			alias = strings.TrimSpace(alias)
			if isLimitedText(alias, maxNameRunes) && normalize(alias) != "" && normalize(alias) != normalize(name) {
				aliases = append(aliases, alias)
			}
		}
		remap[i] = len(part.entities)
		part.entities = append(part.entities, cleanEntity{name: name, kind: kind, aliases: aliases})
	}
	index := func(value flexInt) int {
		if value < 0 || int(value) >= len(remap) {
			return -1
		}
		return remap[value]
	}
	words := tokenSet(text)
	for _, fact := range raw.Facts {
		entity := index(fact.Entity)
		predicate := predicateName(fact.Predicate)
		value := strings.TrimSpace(fact.Value)
		evidence := strings.TrimSpace(fact.Evidence)
		if entity < 0 || predicate == "" || !isLimitedText(value, maxValueRunes) || len(part.facts) == maxFactsPerPart {
			continue
		}
		if !grounded(evidence, words) && !grounded(value, words) {
			continue
		}
		confidence := float64(fact.Confidence)
		if confidence <= 0 || confidence > 1 {
			confidence = 0.5
		}
		validFrom := occurred
		if date, ok := parseDate(fact.ValidFrom); ok && date.Before(occurred.Add(24*time.Hour)) {
			validFrom = date
		}
		part.facts = append(part.facts, cleanFact{
			entity: entity, predicate: predicate, value: value, confidence: confidence, validFrom: validFrom,
			evidence: truncateRunes(evidence, maxEvidenceRunes),
		})
	}
	for _, relation := range raw.Relations {
		from, to := index(relation.From), index(relation.To)
		predicate := predicateName(relation.Predicate)
		if from < 0 || to < 0 || from == to || predicate == "" || len(part.relations) == maxRelationsPerPart {
			continue
		}
		confidence := float64(relation.Confidence)
		if confidence <= 0 || confidence > 1 {
			confidence = 0.5
		}
		part.relations = append(part.relations, cleanRelation{from: from, to: to, predicate: predicate, confidence: confidence})
	}
	return part
}

func parseDate(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	for _, layout := range []string{time.RFC3339, "2006-01-02", "2006-01", "2006"} {
		if parsed, err := time.Parse(layout, value); err == nil && parsed.Year() >= 1970 {
			return parsed.UTC(), true
		}
	}
	return time.Time{}, false
}

// merge joins the parts of one document: entities by normalized name,
// repeated facts and relations once with their highest confidence.
func merge(parts []cleanPart) cleanPart {
	var merged cleanPart
	byName := map[string]int{}
	type factKey struct {
		entity           int
		predicate, value string
	}
	type relationKey struct {
		from, to  int
		predicate string
	}
	facts := map[factKey]int{}
	relations := map[relationKey]int{}
	for _, part := range parts {
		local := make([]int, len(part.entities))
		for i, entity := range part.entities {
			key := normalize(entity.name)
			at, ok := byName[key]
			if !ok {
				at = len(merged.entities)
				byName[key] = at
				merged.entities = append(merged.entities, cleanEntity{name: entity.name, kind: entity.kind})
			}
			target := &merged.entities[at]
			if target.kind == "other" && entity.kind != "other" {
				target.kind = entity.kind
			}
			for _, alias := range entity.aliases {
				if !slices.ContainsFunc(target.aliases, func(existing string) bool { return normalize(existing) == normalize(alias) }) {
					target.aliases = append(target.aliases, alias)
				}
			}
			local[i] = at
		}
		for _, fact := range part.facts {
			fact.entity = local[fact.entity]
			key := factKey{fact.entity, fact.predicate, normalize(fact.value)}
			if at, ok := facts[key]; ok {
				merged.facts[at].confidence = max(merged.facts[at].confidence, fact.confidence)
				continue
			}
			facts[key] = len(merged.facts)
			merged.facts = append(merged.facts, fact)
		}
		for _, relation := range part.relations {
			relation.from, relation.to = local[relation.from], local[relation.to]
			key := relationKey{relation.from, relation.to, relation.predicate}
			if at, ok := relations[key]; ok {
				merged.relations[at].confidence = max(merged.relations[at].confidence, relation.confidence)
				continue
			}
			relations[key] = len(merged.relations)
			merged.relations = append(merged.relations, relation)
		}
	}
	return merged
}

// extractPart asks the model about one part. An answer that does not decode,
// usually one cut off at the output limit, is asked again in two halves.
func (e *Engine) extractPart(ctx context.Context, document upstream.GraphDocument, part section, index, total int,
	vocabulary []string, occurred time.Time, depth int) ([]cleanPart, error) {
	hints, err := e.knownEntities(ctx, part.Text)
	if err != nil {
		return nil, err
	}
	var raw Extraction
	err = e.model.JSON(ctx, "extraction", extractionSchema, extractionSystem(vocabulary),
		extractionInput(document, part, index, total, hints), &raw)
	var providerErr *llm.Error
	if err != nil && !errors.As(err, &providerErr) && ctx.Err() == nil && depth < 2 && len(part.Text) > 2000 {
		var parts []cleanPart
		for _, half := range splitSections(part.Text, len(part.Text)/2+1) {
			half.Heading = part.Heading
			cleaned, err := e.extractPart(ctx, document, half, index, total, vocabulary, occurred, depth+1)
			if err != nil {
				return nil, err
			}
			parts = append(parts, cleaned...)
		}
		return parts, nil
	}
	if err != nil {
		return nil, providerError("extract document", err)
	}
	return []cleanPart{clean(raw, part.Text, occurred)}, nil
}

// providerError turns a model provider failure into a dependency error that
// names the refusal.
func providerError(operation string, err error) error {
	var providerErr *llm.Error
	if !errors.As(err, &providerErr) {
		return err
	}
	return &upstream.DependencyError{
		Dependency: "provider", Operation: operation, Status: providerErr.Status,
		Retryable: providerErr.Retryable(), RetryAfter: providerErr.RetryAfter,
		Refusal: providerErr.Class, Cause: err,
	}
}

// knownPredicates lists the predicates the index uses most, for the prompt.
func (e *Engine) knownPredicates(ctx context.Context) ([]string, error) {
	e.vocabularyMu.Lock()
	defer e.vocabularyMu.Unlock()
	if e.vocabulary != nil && time.Since(e.vocabularyAt) < 10*time.Minute {
		return e.vocabulary, nil
	}
	rows, err := e.db.QueryContext(ctx, `
		SELECT predicate FROM facts WHERE status <> 'retracted'
		GROUP BY predicate ORDER BY count(*) DESC, predicate LIMIT 60`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var vocabulary []string
	for rows.Next() {
		var predicate string
		if err := rows.Scan(&predicate); err != nil {
			return nil, err
		}
		vocabulary = append(vocabulary, predicate)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, predicate := range defaultPredicates {
		if len(vocabulary) >= 60 {
			break
		}
		if !slices.Contains(vocabulary, predicate) {
			vocabulary = append(vocabulary, predicate)
		}
	}
	e.vocabulary, e.vocabularyAt = vocabulary, time.Now()
	return vocabulary, nil
}

// knownEntities lists stored entities that the text names, by name or
// alias, so the model reuses their names.
func (e *Engine) knownEntities(ctx context.Context, text string) ([]entityHint, error) {
	query := ftsQuery(text, 300)
	if query == "" {
		return nil, nil
	}
	rows, err := e.db.QueryContext(ctx, `
		SELECT e.rid, e.name, e.norm, e.kind
		FROM (SELECT rowid, rank FROM entities_fts WHERE entities_fts MATCH ? ORDER BY rank LIMIT 200) m
		JOIN entities e ON e.rid = m.rowid
		ORDER BY m.rank`, query)
	if err != nil {
		return nil, err
	}
	type candidate struct {
		rid              int64
		name, norm, kind string
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.rid, &c.name, &c.norm, &c.kind); err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	padded := " " + normalize(text) + " "
	mentions := func(norm string) bool { return norm != "" && strings.Contains(padded, " "+norm+" ") }
	var hints []entityHint
	for _, c := range candidates {
		named := mentions(c.norm)
		if !named {
			aliases, err := e.db.QueryContext(ctx, `SELECT norm FROM entity_aliases WHERE entity_rid = ?`, c.rid)
			if err != nil {
				return nil, err
			}
			for aliases.Next() && !named {
				var alias string
				if err := aliases.Scan(&alias); err != nil {
					aliases.Close()
					return nil, err
				}
				named = mentions(alias)
			}
			aliases.Close()
		}
		if named {
			hints = append(hints, entityHint{name: c.name, kind: c.kind})
			if len(hints) == maxEntityHints {
				break
			}
		}
	}
	return hints, nil
}

func revisionNumber(revision string) (int, error) {
	number, err := strconv.Atoi(strings.TrimPrefix(revision, "r"))
	if err != nil || number < 1 || revision != "r"+strconv.Itoa(number) {
		return 0, fmt.Errorf("invalid document revision %q", revision)
	}
	return number, nil
}

func encodeResult(result upstream.GraphIngestResult) (string, error) {
	data, err := json.Marshal(result)
	return string(data), err
}
