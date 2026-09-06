package membench

// Optional answer stage for the versioned benchmark (E02). The mechanism
// follows the Cognee eval_framework evaluators
// (cognee/eval_framework/evaluation/evaluator_adapters.py and
// cognee/eval_framework/evaluation/metrics/exact_match.py, pinned at
// commit 78ff576559a7f75f65884c5bd90b22cdc790016e): answers are composed
// from the retrieved evidence, graded exactly/structurally where
// possible, and a pluggable judge covers semantic correctness.
// Reimplemented from scratch in Go; no Cognee code is copied.
//
// The stage's separations are the contract:
//
//   - RETRIEVAL metrics stay in RunReport (report.go); this file only
//     reuses the same retrieval to compose answers.
//   - Citation EXISTENCE (do the cited IDs exist among the
//     retrieved/known facts) is scored separately from claim SUPPORT
//     (does the cited evidence actually support the claim). A real but
//     unrelated citation passes existence and fails support.
//   - UNANSWERABLE examples (empty gold set) grade ABSTENTION, not the
//     answer; answerable questions whose gold was not retrieved are
//     RETRIEVAL MISSES, counted separately from both - a retrieval miss
//     is never a correct unanswerable classification.
//   - Answer accuracy, evidence metrics and COST (token usage) are
//     reported in separate summary fields with per-case artifacts.
//
// The grading contract (see AnswerSummary): accuracy, citation
// existence, claim support and abstention are SEPARATE outcomes, each
// with an explicit denominator recorded next to its rate. Accuracy rates
// exist in an end-to-end form (over every answerable case, so
// abstentions, retrieval misses and failures depress them instead of
// silently shrinking them) and, where meaningful, a conditional
// answered-only form. A judge call that errors, is malformed, or never
// happens because the token budget is spent is recorded as unavailable
// with a per-case reason and counted - never a successful grade, never a
// dropped sample.
//
// Model calls are never default: the extractive composer and the gold
// judge are deterministic and offline; a model-backed composer (reflect)
// or judge (OpenAI-compatible chat) requires explicit endpoint, model and
// token-budget configuration, and the stage always runs against the
// caller's store - for the CLI that is a throwaway in-memory database in
// the membench namespace, never the live memory namespace. The report
// records per-case outcomes and aggregate rates only: it never declares
// a winner across composer x judge combinations.

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	stdreflect "reflect"
	"strings"

	"github.com/hypervisor-io/punk-records/internal/llm"
	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/reflect"
)

// AnswerRecord is one scenario line for the answer stage: the base
// fact/query Record plus the expected answer used for exact/structured
// grading. The base Record is untouched, so BaseRecords + CorpusHash keep
// byte-identical corpus fingerprints with the E01 fixtures.
type AnswerRecord struct {
	Record
	// Answer is the expected answer text (may be JSON for structured
	// grading). Empty: exact grading is skipped for this query.
	Answer string `json:"answer,omitempty"`
}

// LoadAnswers reads an answer-aware JSONL scenario file, skipping blank
// lines. Same format as Load plus the optional "answer" field.
func LoadAnswers(path string) ([]AnswerRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var recs []AnswerRecord
	scanner := bufio.NewScanner(f)
	line := 0
	for scanner.Scan() {
		line++
		text := scanner.Text()
		if len(text) == 0 {
			continue
		}
		var r AnswerRecord
		if err := json.Unmarshal([]byte(text), &r); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		recs = append(recs, r)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return recs, nil
}

// BaseRecords strips the answer labels: the plain Records the retrieval
// suite (and its corpus hash) always saw.
func BaseRecords(arecs []AnswerRecord) []Record {
	out := make([]Record, len(arecs))
	for i, a := range arecs {
		out[i] = a.Record
	}
	return out
}

// answerScenarioHash fingerprints the full answer scenario, expected
// answers included: a changed answer label changes the answer report's
// scenario hash even when the retrieval corpus hash is unchanged.
func answerScenarioHash(arecs []AnswerRecord) (string, error) {
	b, err := json.Marshal(arecs)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// ComposeResult is one composed answer with everything the report needs
// to grade it.
type ComposeResult struct {
	Text     string   `json:"text"`
	CitedIDs []string `json:"cited_ids"` // fact/model IDs the answer claims as support
	KnownIDs []string `json:"known_ids"` // IDs the composer was actually shown (existence base)
	// ShownFacts carries the full bodies of the evidence the composer
	// actually showed its model (reflect's tool results): the judge
	// resolves citations against these exact bodies, so resolution is
	// COMPLETE regardless of namespace size and never re-scans the
	// namespace behind a capped query. A carried body is admissible
	// only when its Namespace matches the stage's namespace and its ID
	// is among KnownIDs (actually shown): foreign-namespace or unshown
	// bodies resolve to nothing. Optional: a composer that does not
	// carry bodies leaves it nil, and its known-but-uncarried IDs
	// stay unresolved for support grading.
	ShownFacts []memory.Fact `json:"shown_facts,omitempty"`
	Abstained  bool          `json:"abstained"` // composer declined: no evidence for a claim
	Tokens     int           `json:"tokens"`    // measured composer token usage (0 when deterministic)
	// UsageUnknown counts this composition's model calls (failed calls
	// and failed loops included) whose responses reported no token
	// usage. Unknown is not zero: the runner propagates the exact
	// per-call count into the summary instead of collapsing a
	// multi-call loop into at most one unknown call.
	UsageUnknown int `json:"usage_unknown,omitempty"`
}

// Composer turns a question and its retrieved facts into an answer. The
// default is the deterministic extractive composer; a model-backed
// composer (reflect loop) plugs in through the same interface.
// Implementations that make model calls MUST return the usage consumed
// so far in ComposeResult.Tokens even when they return an error (e.g. a
// multi-call loop stopped by a budget): spent budget is never dropped
// with the error. Multi-call implementations SHOULD likewise report
// their per-call unknown-usage count in ComposeResult.UsageUnknown - a
// loop whose calls partly report no usage is under-measured per call,
// not per composition.
type Composer interface {
	Compose(ctx context.Context, q string, facts []memory.Fact) (ComposeResult, error)
	ComposerConfig() ComposerConfig
}

// boundedComposer is a composer that accepts a per-call measured-usage
// bound (the stage's remaining budget). RunAnswers prefers it over
// Compose whenever a token budget is configured, so a model-backed
// composer's internal multi-call loop is gated before each of its model
// calls, not only before the first.
type boundedComposer interface {
	Composer
	composeWithin(ctx context.Context, q string, facts []memory.Fact, maxTokens int) (ComposeResult, error)
}

// ComposerConfig is recorded in the report: which composer produced the
// answers, and whether it made model calls.
type ComposerConfig struct {
	Provider    string `json:"provider"` // "extractive" | "reflect" | test double name
	Model       string `json:"model,omitempty"`
	Endpoint    string `json:"endpoint,omitempty"`
	ModelBacked bool   `json:"model_backed"`
}

// JudgeEvidence is one cited fact resolved against the stage's retrieval:
// the judge grades support from content, not from the answer's say-so.
type JudgeEvidence struct {
	ID   string `json:"id"`
	Key  string `json:"key"`
	Body string `json:"body"`
}

// JudgeInput is everything a judge may use for one case.
type JudgeInput struct {
	Q              string
	Answer         string
	ExpectedAnswer string          // may be empty
	Evidence       []JudgeEvidence // cited facts resolved against retrieval
	CitedCount     int             // total citations (Evidence covers the resolvable ones)
	ExpectKeys     []string        // unique gold evidence keys (empty: unanswerable)
	Unanswerable   bool
	// RemainingBudget is the stage's remaining measured token budget when
	// the runner enforces one (0 when untracked). The runner gates before
	// the call; the value lets budget-aware judges bound their own call.
	RemainingBudget int
}

// JudgeVerdict is one judge's grading. A nil field means the judge does
// not grade that dimension for this case; the summary excludes it from
// the corresponding rate instead of treating it as pass or fail.
// Implementations that make model calls MUST return the usage consumed
// in PromptTokens/CompletionTokens even when they return an error (e.g.
// a response that is not a valid verdict): spent budget is never dropped
// with the error.
type JudgeVerdict struct {
	Correct          *bool // semantic correctness of the answer
	Support          *bool // the cited evidence actually supports the claim
	Justification    string
	PromptTokens     int
	CompletionTokens int
}

// Judge grades one answer. The default is the deterministic gold judge;
// a model-backed judge (OpenAI-compatible chat) plugs in through the same
// interface and is only ever constructed with explicit endpoint/model/
// budget configuration.
type Judge interface {
	Judge(ctx context.Context, in JudgeInput) (JudgeVerdict, error)
	JudgeConfig() JudgeConfig
}

// JudgeConfig is recorded in the report: which judge graded the answers,
// with its endpoint, model and configured token budget when model-backed.
// GradesCorrectness/GradesSupport declare the dimensions the judge
// grades: the gold judge grades support only, the model judge grades
// both. The runner uses them to keep a judge whose calls became
// unavailable (error or budget) from silently shrinking the denominator
// of a rate it was responsible for, while never inventing a denominator
// for a dimension the judge does not grade at all.
type JudgeConfig struct {
	Provider          string `json:"provider"` // "gold" | "openai-compatible" | test double name
	Model             string `json:"model,omitempty"`
	Endpoint          string `json:"endpoint,omitempty"`
	BudgetTokens      int    `json:"budget_tokens,omitempty"`
	ModelBacked       bool   `json:"model_backed"`
	GradesCorrectness bool   `json:"grades_correctness,omitempty"`
	GradesSupport     bool   `json:"grades_support,omitempty"`
}

// extractiveComposer is the deterministic default: the top-ranked fact's
// body is the answer and its ID the citation; an empty retrieval is an
// abstention, never an invented claim. No model calls.
type extractiveComposer struct{}

func (extractiveComposer) ComposerConfig() ComposerConfig {
	return ComposerConfig{Provider: "extractive"}
}

func (extractiveComposer) Compose(_ context.Context, _ string, facts []memory.Fact) (ComposeResult, error) {
	if len(facts) == 0 {
		return ComposeResult{Abstained: true}, nil
	}
	ids := make([]string, 0, len(facts))
	for _, f := range facts {
		ids = append(ids, f.ID)
	}
	return ComposeResult{Text: facts[0].Body, CitedIDs: []string{facts[0].ID}, KnownIDs: ids}, nil
}

// reflectComposer adapts the reflect loop (internal/reflect) as a
// model-backed composer: reflect does its own hierarchical evidence
// gathering over the namespace, so the facts argument is ignored; the
// loop's validated citations, abstention flag, shown-evidence set (IDs
// and full bodies) and token usage - measured and per-call unknown - map
// straight onto ComposeResult. Constructing one requires an explicitly
// configured model client.
type reflectComposer struct {
	eng *reflect.Engine
	ns  string
	cfg ComposerConfig
}

// NewReflectComposer wraps a reflect engine as the answer composer. The
// engine must already carry an explicitly configured model client;
// endpoint and model are recorded in the report for provenance.
func NewReflectComposer(eng *reflect.Engine, ns, endpoint, model string) Composer {
	return &reflectComposer{
		eng: eng,
		ns:  ns,
		cfg: ComposerConfig{Provider: "reflect", Model: model, Endpoint: endpoint, ModelBacked: true},
	}
}

func (c *reflectComposer) ComposerConfig() ComposerConfig { return c.cfg }

func (c *reflectComposer) Compose(ctx context.Context, q string, facts []memory.Fact) (ComposeResult, error) {
	return c.compose(ctx, q, 0)
}

// composeWithin is the budgeted entry RunAnswers prefers: the stage's
// remaining budget bounds the reflect loop's measured usage, checked
// before every model call inside the loop.
func (c *reflectComposer) composeWithin(ctx context.Context, q string, _ []memory.Fact, maxTokens int) (ComposeResult, error) {
	return c.compose(ctx, q, maxTokens)
}

func (c *reflectComposer) compose(ctx context.Context, q string, maxTokens int) (ComposeResult, error) {
	ans, err := c.eng.ReflectWith(ctx, c.ns, q, reflect.Opts{MaxTokens: maxTokens})
	if err != nil {
		// Retain the usage and the per-call unknown count the loop
		// consumed before failing (Composer contract): a budget stop or
		// a failed call mid-loop has real spend and real
		// under-measurement, neither dropped with the error.
		return ComposeResult{Tokens: ans.Tokens, UsageUnknown: ans.UsageUnknown}, err
	}
	return ComposeResult{
		Text:         ans.Text,
		CitedIDs:     ans.Citations,
		KnownIDs:     ans.Evidence,
		ShownFacts:   ans.ShownFacts,
		Abstained:    ans.Abstained,
		Tokens:       ans.Tokens,
		UsageUnknown: ans.UsageUnknown,
	}, nil
}

// goldJudge is the deterministic default grader: it never grades
// correctness (that is exact/structured matching's job, or a model
// judge's) and grades claim SUPPORT against the fixture's gold labels.
// Citation membership in the gold key set is gold-citation RELEVANCE,
// not claim support - a false answer citing the gold fact is not
// supported. The verdict is three-way:
//
//   - FALSE, decided by the labels alone: no citations at all, a cited
//     ID that did not resolve to shown evidence (fabricated or outside
//     it), or a cited fact whose key is outside the query's gold set
//     (real but unrelated evidence cannot support the gold claim).
//   - TRUE, deterministically verified: every citation is gold-relevant
//     AND the answer exact/structured-matches the expected answer or a
//     cited gold body (the claim is the gold content, verbatim).
//   - UNGRADED (nil), never a pass: citations are gold-relevant but the
//     answer diverges from the expected answer and every cited gold
//     body. Semantic support of a divergent claim is a model judge's
//     job; the deterministic grader abstains explicitly, with the
//     reason in Justification.
//
// An unanswerable question is never support-graded: abstention is its
// graded signal. No model calls.
type goldJudge struct{}

func (goldJudge) JudgeConfig() JudgeConfig {
	return JudgeConfig{Provider: "gold", GradesSupport: true}
}

func (goldJudge) Judge(_ context.Context, in JudgeInput) (JudgeVerdict, error) {
	var v JudgeVerdict
	if in.Unanswerable || len(in.ExpectKeys) == 0 {
		return v, nil
	}
	switch {
	case in.CitedCount == 0:
		v.Support = boolPtr(false)
		v.Justification = "no cited evidence: an answer without citations is not supported"
		return v, nil
	case len(in.Evidence) != in.CitedCount:
		v.Support = boolPtr(false)
		v.Justification = "at least one citation did not resolve to shown evidence"
		return v, nil
	}
	gold := map[string]bool{}
	for _, k := range in.ExpectKeys {
		gold[k] = true
	}
	for _, e := range in.Evidence {
		if !gold[e.Key] {
			v.Support = boolPtr(false)
			v.Justification = "cited evidence exists but is outside the question's gold evidence set"
			return v, nil
		}
	}
	if in.ExpectedAnswer != "" && answerMatch(in.ExpectedAnswer, in.Answer) {
		v.Support = boolPtr(true)
		return v, nil
	}
	for _, e := range in.Evidence {
		if answerMatch(e.Body, in.Answer) {
			v.Support = boolPtr(true)
			return v, nil
		}
	}
	v.Justification = "citations are gold-relevant but the answer diverges from the expected answer and every cited gold body; semantic support left ungraded"
	return v, nil
}

func boolPtr(b bool) *bool { return &b }

// llmJudge is the model-backed judge: one OpenAI-compatible chat call per
// answered case returning strict verdict JSON. It exists only with an
// explicitly configured client, endpoint and token budget.
type llmJudge struct {
	client llm.Client
	cfg    JudgeConfig
}

// NewLLMJudge builds the model-backed judge over an already-configured
// client; endpoint and budgetTokens are recorded in the report. The
// runner additionally enforces the budget across the whole stage.
func NewLLMJudge(client llm.Client, endpoint string, budgetTokens int) Judge {
	return &llmJudge{
		client: client,
		cfg: JudgeConfig{
			Provider:          "openai-compatible",
			Model:             client.Model(),
			Endpoint:          endpoint,
			BudgetTokens:      budgetTokens,
			ModelBacked:       true,
			GradesCorrectness: true,
			GradesSupport:     true,
		},
	}
}

func (j *llmJudge) JudgeConfig() JudgeConfig { return j.cfg }

const judgeSystemPrompt = `You grade one benchmark answer. Reply with ONLY a JSON object:
{"correct": true|false, "support": true|false, "justification": "one sentence"}
- correct: the answer semantically answers the question, given the expected answer (when provided) and the evidence.
- support: the cited evidence actually supports the answer's claims. An existing but unrelated citation is NOT support. An answer with no cited evidence is NOT supported.
- A confident answer to an unanswerable question is not correct and not supported.`

// stripJSONFence strips an optional Markdown code fence around a judge
// reply (mirrors the fence-strip in cmd/punk/main.go's obsSummarizer and
// sibling LLM adapters): find the first "```", drop it and an optional
// "json" language tag, cut at the next "```", then trim. A reply with no
// fence passes through unchanged (after trimming).
func stripJSONFence(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "```"); i >= 0 {
		s = s[i+3:]
		s = strings.TrimPrefix(s, "json")
		if j := strings.Index(s, "```"); j >= 0 {
			s = s[:j]
		}
	}
	return strings.TrimSpace(s)
}

func (j *llmJudge) Judge(ctx context.Context, in JudgeInput) (JudgeVerdict, error) {
	evidence, err := json.Marshal(in.Evidence)
	if err != nil {
		return JudgeVerdict{}, err
	}
	user := map[string]any{
		"question":        in.Q,
		"answer":          in.Answer,
		"expected_answer": in.ExpectedAnswer,
		"unanswerable":    in.Unanswerable,
		"cited_count":     in.CitedCount,
		"cited_evidence":  json.RawMessage(evidence),
	}
	ub, err := json.Marshal(user)
	if err != nil {
		return JudgeVerdict{}, err
	}
	res, err := j.client.Chat(ctx, []llm.Turn{
		{Role: "system", Content: judgeSystemPrompt},
		{Role: "user", Content: string(ub)},
	}, nil)
	if err != nil {
		return JudgeVerdict{}, err
	}
	// The usage the call consumed travels with every grading failure
	// below (Judge contract): a malformed verdict still spent tokens.
	v := JudgeVerdict{PromptTokens: res.PromptTokens, CompletionTokens: res.CompletionTokens}
	var body struct {
		Correct       *bool  `json:"correct"`
		Support       *bool  `json:"support"`
		Justification string `json:"justification"`
	}
	if err := json.Unmarshal([]byte(stripJSONFence(res.Content)), &body); err != nil {
		return v, fmt.Errorf("membench: judge verdict is not verdict JSON: %w", err)
	}
	// A parseable but incomplete verdict ({} or one dimension missing) is
	// a judge FAILURE, not a successful null grade: the model judge's
	// contract is both dimensions, so a missing one means the response is
	// not a verdict.
	if body.Correct == nil || body.Support == nil {
		return v, fmt.Errorf("membench: judge verdict incomplete: correct and support are both required (got correct=%v support=%v)", body.Correct != nil, body.Support != nil)
	}
	v.Correct = body.Correct
	v.Support = body.Support
	v.Justification = body.Justification
	return v, nil
}

// AnswerOptions configures the answer stage.
type AnswerOptions struct {
	K    int    // top-k retrieved per query for composition (<=0 defaults to 5)
	Mode string // "cold" (ingest then answer; default) | "warm" (corpus already ingested, e.g. by the suite)
	// Composer defaults to the deterministic extractive composer; Judge
	// defaults to the deterministic gold judge. Both defaults are offline
	// and make no model calls.
	Composer Composer
	Judge    Judge
	// MaxTokens is the hard token budget across composer and judge calls
	// for the whole stage. REQUIRED (>0) when any wired component is
	// model-backed; 0 with deterministic defaults is the normal offline
	// run. The budget is checked before EVERY model call - before each
	// composition, before each judge grading, and (via boundedComposer /
	// reflect.Opts.MaxTokens) before every call inside the reflect
	// loop, which receives the remaining budget. A call already in flight
	// may overshoot the budget by its own usage (bounded overshoot: the
	// gate is a pre-call check, not a preemption); no further call starts
	// afterwards. Usage consumed by calls whose grading then fails is
	// retained and counted. Cases whose composer or judge call the budget
	// blocks are recorded with an explicit budget error and counted in
	// Failed - never dropped, never silently over budget. Calls whose
	// endpoint reports no usage are counted PER CALL in UsageUnknown
	// (unknown is not zero), a multi-call composer's unreported calls
	// included; the budget bounds MEASURED usage, so the count makes an
	// under-measured budget visible.
	MaxTokens int
}

// AnswerCaseResult is the persisted per-query answer artifact: the
// answer, the retrieved and cited IDs, the abstention decision, the
// grading outcomes, the token usage and, for a failed case, the error.
// Tokens/JudgeTokens are VOLATILE (measured cost: stripped by
// Report.Stable).
type AnswerCaseResult struct {
	Q              string   `json:"q"`
	ExpectedAnswer string   `json:"expected_answer,omitempty"`
	Answer         string   `json:"answer"`
	Abstained      bool     `json:"abstained"`
	Unanswerable   bool     `json:"unanswerable"`
	RetrievedIDs   []string `json:"retrieved_ids"`
	RetrievedKeys  []string `json:"retrieved_keys"`
	CitedIDs       []string `json:"cited_ids"`
	// CitationExist is true when every cited ID exists among the
	// retrieved IDs or the composer's known IDs. False when the answer
	// carries at least one unknown ID, and when there are no citations.
	CitationExist bool `json:"citation_exist"`
	// ExactMatch is the deterministic grader (normalized string or
	// structured JSON equality against the expected answer); nil when the
	// case has no expected answer or was abstained/failed.
	ExactMatch *bool `json:"exact_match,omitempty"`
	// JudgeCorrect is the judge's semantic correctness verdict; nil when
	// the judge did not grade correctness for this case.
	JudgeCorrect *bool `json:"judge_correct,omitempty"`
	// Support is the claim-support verdict (gold labels or the judge);
	// nil when the case was not support-graded (unanswerable, abstained,
	// failed, or the deterministic grader explicitly declined a semantic
	// verdict - see Note).
	Support *bool `json:"support,omitempty"`
	// Note carries the grader's per-case remark: the gold judge's reason
	// for a false/ungraded support verdict, or a model judge's
	// justification. VOLATILE for model graders (stripped by Stable).
	Note        string `json:"note,omitempty"`
	Tokens      int    `json:"tokens"`
	JudgeTokens int    `json:"judge_tokens,omitempty"`
	Error       string `json:"error,omitempty"`
}

// AnswerSummary is the aggregate over the stage's cases. Every rate's
// denominator is recorded next to it as an explicit count, and a rate is
// nil (omitted) when its denominator is zero - never a vacuous 0 or 1:
//
//   - ExactMatchRate is the END-TO-END exact accuracy: exact matches over
//     ExactAnswerable (every answerable case with an expected answer).
//     Abstentions, retrieval misses, composer/judge failures and
//     budget-blocked cases all count as not matched, so they depress the
//     rate instead of silently shrinking its denominator.
//     AnsweredExactMatchRate is the CONDITIONAL form over ExactGraded
//     (answered cases with an exact verdict) for callers that want the
//     answered-only view explicitly labeled as such.
//   - JudgeCorrectRate averages over CorrectGraded plus, when the judge
//     grades correctness, JudgeErrors: a judge whose calls fail or are
//     budget-blocked depresses the rate it was responsible for instead of
//     vanishing from it. SupportRate is the same shape over
//     SupportGraded. CitationExistenceRate averages over ExistenceGraded
//     (answered cases with >=1 citation).
//
// Unanswerable cases grade abstention: ConfidentUnanswerable counts the
// abstention failures (the red-proof violation), AbstainedUnanswerable
// the correct abstentions, AbstainedAnswerable the answerable cases the
// composer declined (retrieval misses included - a miss is not a correct
// unanswerable classification). JudgeErrors counts answered answerable
// cases whose judge grading was unavailable (call error, malformed
// verdict, or budget-blocked): those cases keep a per-case Error reason
// and are never successful grades or dropped samples. Tokens and
// JudgeTokens are the measured cost (usage of failed calls retained),
// kept apart from the accuracy and evidence rates; UsageUnknown counts,
// per model call, the model-backed calls whose endpoint reported no
// usage - unknown, which is not zero.
type AnswerSummary struct {
	Queries         int `json:"queries"`
	Answerable      int `json:"answerable"`
	Unanswerable    int `json:"unanswerable"`
	RetrievalMisses int `json:"retrieval_misses"`
	Answered        int `json:"answered"`
	Abstained       int `json:"abstained"`
	Failed          int `json:"failed"`
	// Denominator counts for the rates below (see the contract above).
	ExactAnswerable int `json:"exact_answerable"`
	ExactGraded     int `json:"exact_graded"`
	CorrectGraded   int `json:"correct_graded"`
	SupportGraded   int `json:"support_graded"`
	ExistenceGraded int `json:"existence_graded"`
	JudgeErrors     int `json:"judge_errors"`

	ExactMatchRate         *float64 `json:"exact_match_rate,omitempty"`
	AnsweredExactMatchRate *float64 `json:"answered_exact_match_rate,omitempty"`
	JudgeCorrectRate       *float64 `json:"judge_correct_rate,omitempty"`
	CitationExistenceRate  *float64 `json:"citation_existence_rate,omitempty"`
	SupportRate            *float64 `json:"support_rate,omitempty"`

	AbstainedUnanswerable int `json:"abstained_unanswerable"`
	ConfidentUnanswerable int `json:"confident_unanswerable"`
	AbstainedAnswerable   int `json:"abstained_answerable"`
	Tokens                int `json:"tokens"`
	JudgeTokens           int `json:"judge_tokens"`
	// UsageUnknown counts model-backed composer/judge model calls (failed
	// ones included) whose endpoint reported no token usage. A multi-call
	// composer contributes its exact per-call count (propagated through
	// ComposeResult.UsageUnknown), never collapsed into at most one
	// unknown call per composition. VOLATILE like the token fields
	// (stripped by Report.Stable).
	UsageUnknown int `json:"usage_unknown"`
}

// AnswerReport is the answer stage's section of the versioned Report:
// composer and judge configuration, the token budget, the aggregate and
// every per-case artifact.
type AnswerReport struct {
	ScenarioSHA256 string             `json:"scenario_sha256"`
	Composer       ComposerConfig     `json:"composer"`
	Judge          JudgeConfig        `json:"judge"`
	BudgetTokens   int                `json:"budget_tokens,omitempty"`
	Summary        AnswerSummary      `json:"summary"`
	Cases          []AnswerCaseResult `json:"cases"`
}

// RunAnswers runs the answer stage over one scenario: per query, retrieve
// (offline FTS/hybrid like RunDetailed), compose an answer, then grade
// it - exact/structured match where an expected answer exists, the judge
// for support and (model judges) semantic correctness. Retrieval errors,
// composer errors, judge errors and budget exhaustion are per-case
// outcomes counted in Failed, never run-fatal and never silently dropped.
// Ingest errors remain run-fatal. A model-backed composer or judge
// without an explicit MaxTokens budget is a configuration error.
func RunAnswers(ctx context.Context, s *memory.Store, ns string, recs []AnswerRecord, o AnswerOptions) (AnswerReport, error) {
	k := o.K
	if k <= 0 {
		k = 5
	}
	mode := o.Mode
	if mode == "" {
		mode = "cold"
	}
	if mode != "cold" && mode != "warm" {
		return AnswerReport{}, fmt.Errorf("membench: answers mode %q: want cold or warm", mode)
	}
	composer := o.Composer
	if composer == nil {
		composer = extractiveComposer{}
	}
	judge := o.Judge
	if judge == nil {
		judge = goldJudge{}
	}
	ccfg, jcfg := composer.ComposerConfig(), judge.JudgeConfig()
	if (ccfg.ModelBacked || jcfg.ModelBacked) && o.MaxTokens <= 0 {
		return AnswerReport{}, fmt.Errorf("membench: model-backed answer stage (composer %q / judge %q) requires an explicit token budget (MaxTokens > 0)", ccfg.Provider, jcfg.Provider)
	}
	hash, err := answerScenarioHash(recs)
	if err != nil {
		return AnswerReport{}, err
	}
	rep := AnswerReport{
		ScenarioSHA256: hash,
		Composer:       ccfg,
		Judge:          jcfg,
		BudgetTokens:   o.MaxTokens,
	}
	if mode == "cold" {
		for _, r := range recs {
			if r.Type != "fact" {
				continue
			}
			if _, err := s.Write(ctx, memory.WriteInput{Namespace: ns, Key: r.Key, Body: r.Body, Writer: "membench"}); err != nil {
				return AnswerReport{}, fmt.Errorf("ingest %s: %w", r.Key, err)
			}
		}
	}

	var sum AnswerSummary
	spent := 0
	var exactNum, correctNum, existNum, supportNum int
	for _, r := range recs {
		if r.Type != "query" {
			continue
		}
		sum.Queries++
		cr := AnswerCaseResult{Q: r.Q, ExpectedAnswer: r.Answer}
		expect := map[string]bool{}
		for _, e := range r.Expect {
			expect[e] = true
		}
		cr.Unanswerable = len(expect) == 0
		if cr.Unanswerable {
			sum.Unanswerable++
		} else {
			sum.Answerable++
			if r.Answer != "" {
				// The end-to-end exact denominator: every answerable
				// labeled case, counted before any outcome is known, so
				// abstention, retrieval miss and failure can never
				// silently shrink it.
				sum.ExactAnswerable++
			}
		}

		facts, err := s.HybridSearch(ctx, ns, r.Q, k, 0)
		if err != nil {
			cr.Error = err.Error()
			sum.Failed++
			rep.Cases = append(rep.Cases, cr)
			continue
		}
		byID := map[string]memory.Fact{}
		goldRetrieved := false
		for _, f := range facts {
			cr.RetrievedIDs = append(cr.RetrievedIDs, f.ID)
			cr.RetrievedKeys = append(cr.RetrievedKeys, f.Key)
			byID[f.ID] = f
			if expect[f.Key] {
				goldRetrieved = true
			}
		}
		if !cr.Unanswerable && !goldRetrieved {
			sum.RetrievalMisses++
		}

		// Budget gate before the composer MODEL call (a deterministic
		// composer consumes nothing and is never blocked).
		if ccfg.ModelBacked && spent >= o.MaxTokens {
			cr.Error = fmt.Sprintf("compose: answer token budget exhausted (%d of %d spent)", spent, o.MaxTokens)
			sum.Failed++
			rep.Cases = append(rep.Cases, cr)
			continue
		}
		var cres ComposeResult
		if bc, ok := composer.(boundedComposer); ok && o.MaxTokens > 0 {
			cres, err = bc.composeWithin(ctx, r.Q, facts, o.MaxTokens-spent)
		} else {
			cres, err = composer.Compose(ctx, r.Q, facts)
		}
		// Consumed usage is retained even when the compose failed partway
		// through its model calls (Composer contract).
		cr.Tokens = cres.Tokens
		spent += cres.Tokens
		sum.Tokens += cres.Tokens
		if ccfg.ModelBacked {
			// A composer that tracks unknown usage per model call (a
			// multi-call loop like reflect, failed loops included)
			// propagates its exact count; anything else that reports no
			// measured usage at all is one unknown call. The per-call
			// count is never re-heuristicked on top (no double count).
			if cres.UsageUnknown > 0 {
				sum.UsageUnknown += cres.UsageUnknown
			} else if cres.Tokens == 0 {
				sum.UsageUnknown++
			}
		}
		if err != nil {
			cr.Error = "compose: " + err.Error()
			sum.Failed++
			rep.Cases = append(rep.Cases, cr)
			continue
		}
		cr.Answer, cr.Abstained = cres.Text, cres.Abstained
		cr.CitedIDs = cres.CitedIDs

		if cr.Abstained {
			sum.Abstained++
			if cr.Unanswerable {
				sum.AbstainedUnanswerable++
			} else {
				sum.AbstainedAnswerable++
			}
			rep.Cases = append(rep.Cases, cr)
			continue
		}
		if cr.Unanswerable {
			// A confident non-answer on an unanswerable question is the
			// abstention failure: counted, never graded as an answer.
			sum.ConfidentUnanswerable++
			rep.Cases = append(rep.Cases, cr)
			continue
		}
		sum.Answered++

		// Citation existence: every cited ID must be among the retrieved
		// IDs or the composer's own shown/known IDs.
		known := map[string]bool{}
		for _, id := range cr.RetrievedIDs {
			known[id] = true
		}
		for _, id := range cres.KnownIDs {
			known[id] = true
		}
		if len(cr.CitedIDs) > 0 {
			sum.ExistenceGraded++
			cr.CitationExist = true
			for _, id := range cr.CitedIDs {
				if !known[id] {
					cr.CitationExist = false
				}
			}
			if cr.CitationExist {
				existNum++
			}
		}

		// Deterministic exact/structured grading (no model call, always
		// available on an answered case).
		if r.Answer != "" {
			m := answerMatch(r.Answer, cr.Answer)
			cr.ExactMatch = &m
			sum.ExactGraded++
			if m {
				exactNum++
			}
		}

		// Judge evidence: resolve citations against the initial retrieval
		// first, then - for an ID the composer actually showed its model
		// that the initial top-k missed (reflect's model/observation
		// layer) - against the shown evidence BODIES the composer carried
		// out (ComposeResult.ShownFacts): the exact facts the model saw,
		// resolved completely no matter how large the namespace is. The
		// stage never re-scans the namespace behind a capped query, and
		// resolution never leaves it: a fabricated ID, or one living only
		// in another namespace, resolves to nothing. The judge sees
		// resolved evidence, never the composer's claims about it.
		// A carried body is admissible only when it lives in the
		// stage's own namespace AND the composer reported its ID as
		// actually shown (KnownIDs): a composer bound to a foreign
		// namespace, or one carrying bodies it never claimed to show,
		// resolves to nothing - exactly like a fabricated ID.
		shownIDs := make(map[string]bool, len(cres.KnownIDs))
		for _, id := range cres.KnownIDs {
			shownIDs[id] = true
		}
		shown := make(map[string]memory.Fact, len(cres.ShownFacts))
		for _, f := range cres.ShownFacts {
			if f.Namespace != ns || !shownIDs[f.ID] {
				continue
			}
			shown[f.ID] = f
		}
		evidence := make([]JudgeEvidence, 0, len(cr.CitedIDs))
		for _, id := range cr.CitedIDs {
			f, ok := byID[id]
			if !ok {
				f, ok = shown[id]
			}
			if ok {
				evidence = append(evidence, JudgeEvidence{ID: f.ID, Key: f.Key, Body: f.Body})
			}
		}

		// Budget gate before the judge MODEL call, after the free
		// deterministic grading above: a spent budget blocks the judge
		// (recorded as an unavailable grading, counted in JudgeErrors)
		// but never erases the composed answer or the exact/existence
		// outcomes already graded.
		if jcfg.ModelBacked && spent >= o.MaxTokens {
			cr.Error = fmt.Sprintf("judge: answer token budget exhausted (%d of %d spent)", spent, o.MaxTokens)
			sum.Failed++
			sum.JudgeErrors++
			rep.Cases = append(rep.Cases, cr)
			continue
		}
		keys := make([]string, 0, len(expect))
		for k2 := range expect {
			keys = append(keys, k2)
		}
		remaining := 0
		if o.MaxTokens > 0 {
			remaining = o.MaxTokens - spent
		}
		verdict, err := judge.Judge(ctx, JudgeInput{
			Q:               r.Q,
			Answer:          cr.Answer,
			ExpectedAnswer:  r.Answer,
			Evidence:        evidence,
			CitedCount:      len(cr.CitedIDs),
			ExpectKeys:      keys,
			Unanswerable:    cr.Unanswerable,
			RemainingBudget: remaining,
		})
		// Usage is retained even when the response failed grading (Judge
		// contract): a malformed verdict still spent tokens.
		jt := verdict.PromptTokens + verdict.CompletionTokens
		cr.JudgeTokens = jt
		spent += jt
		sum.JudgeTokens += jt
		if jcfg.ModelBacked && jt == 0 {
			sum.UsageUnknown++
		}
		if err != nil {
			cr.Error = "judge: " + err.Error()
			sum.Failed++
			sum.JudgeErrors++
			rep.Cases = append(rep.Cases, cr)
			continue
		}
		cr.JudgeCorrect = verdict.Correct
		cr.Support = verdict.Support
		cr.Note = verdict.Justification
		if verdict.Correct != nil {
			sum.CorrectGraded++
			if *verdict.Correct {
				correctNum++
			}
		}
		if verdict.Support != nil {
			sum.SupportGraded++
			if *verdict.Support {
				supportNum++
			}
		}
		rep.Cases = append(rep.Cases, cr)
	}
	if sum.ExactAnswerable > 0 {
		v := float64(exactNum) / float64(sum.ExactAnswerable)
		sum.ExactMatchRate = &v
	}
	if sum.ExactGraded > 0 {
		v := float64(exactNum) / float64(sum.ExactGraded)
		sum.AnsweredExactMatchRate = &v
	}
	correctDen := sum.CorrectGraded
	if jcfg.GradesCorrectness {
		correctDen += sum.JudgeErrors
	}
	if correctDen > 0 {
		v := float64(correctNum) / float64(correctDen)
		sum.JudgeCorrectRate = &v
	}
	supportDen := sum.SupportGraded
	if jcfg.GradesSupport {
		supportDen += sum.JudgeErrors
	}
	if supportDen > 0 {
		v := float64(supportNum) / float64(supportDen)
		sum.SupportRate = &v
	}
	if sum.ExistenceGraded > 0 {
		v := float64(existNum) / float64(sum.ExistenceGraded)
		sum.CitationExistenceRate = &v
	}
	rep.Summary = sum
	return rep, nil
}

// answerMatch is the deterministic grader: true when the normalized
// strings are equal (case-insensitive, whitespace collapsed) or when both
// sides parse as JSON and are structurally equal (key order and
// formatting free). Anything else is false - no similarity scoring, no
// model.
func answerMatch(expected, got string) bool {
	if normalizeAnswer(expected) == normalizeAnswer(got) {
		return true
	}
	var e, g any
	if json.Unmarshal([]byte(expected), &e) == nil && json.Unmarshal([]byte(got), &g) == nil {
		return stdreflect.DeepEqual(e, g)
	}
	return false
}

func normalizeAnswer(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}
