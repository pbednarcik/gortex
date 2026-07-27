package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph"
)

func prescribedBodyTargets(source string) (string, []exploreTarget) {
	const symbol = "storage/disk.go::DiskStorage.Load"
	primary := &graph.Node{
		ID: symbol, Name: "DiskStorage.Load", Kind: graph.KindMethod,
		FilePath: "storage/disk.go", StartLine: 12,
	}
	sibling := &graph.Node{
		ID: "storage/cache.go::Cache.Warm", Name: "Cache.Warm", Kind: graph.KindMethod,
		FilePath: "storage/cache.go", StartLine: 40,
	}
	return symbol, []exploreTarget{
		{node: primary, score: 1, source: source},
		{node: sibling, score: 0.4, source: "func (c *Cache) Warm() error { return nil }"},
	}
}

func decodeLocalizationEnvelope(t *testing.T, result *mcpgo.CallToolResult) localizationExploreEnvelope {
	t.Helper()
	text, ok := singleTextContent(result)
	if !ok {
		t.Fatalf("expected one text result: %#v", result)
	}
	var envelope localizationExploreEnvelope
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		t.Fatalf("decode localization envelope: %v\n%s", err, text)
	}
	return envelope
}

// Body and instruction move together. A single-target prescription whose body
// this page now ships owes no read at all — but it claims nothing about
// ranking either, so it releases rather than declaring a proven answer.
func TestASingleTargetPrescriptionShipsItsBodyAndReleasesTheRead(t *testing.T) {
	const task = "loading a stored record leaves the path empty"
	symbol, targets := prescribedBodyTargets(
		`func (d *DiskStorage) Load(path string) ([]byte, error) { return os.ReadFile(path) }`)

	result, _, _, finalized := buildLocalizationExploreResultForTaskFinalized(
		newLocalizationRefinementCompletion(symbol), task, targets, 1600)
	envelope := decodeLocalizationEnvelope(t, result)

	if finalized.State != localizationStateAnswerReady || finalized.Enforceable {
		t.Fatalf("a satisfied single-target prescription did not become a bounded conclusion: %#v", finalized)
	}
	if !envelope.Terminal || !strings.HasPrefix(finalized.FinalResponse, localizationBoundedHeading+"\n") {
		t.Fatalf("a released prescription did not publish its bounded-evidence page: %#v", finalized)
	}
	if finalized.AllowedToolCalls != 0 || strings.Contains(finalized.Instruction, "read(") {
		t.Fatalf("a released prescription still asks for a call: %#v", finalized)
	}
	var carried bool
	for _, evidence := range envelope.Evidence {
		if evidence.ID == symbol && strings.TrimSpace(evidence.Source) != "" {
			carried = true
		}
	}
	if !carried {
		t.Fatal("the read was released without the body that justifies releasing it")
	}
}

// A refinement that authorizes several candidates is asking the caller to pick
// between them. Shipping one of those bodies does not answer that question, so
// the page keeps its bounded read and spends no bytes on a partial answer.
func TestARefinementWithACandidateChoiceKeepsItsRead(t *testing.T) {
	const task = "loading a stored record leaves the path empty"
	symbol, targets := prescribedBodyTargets(
		`func (d *DiskStorage) Load(path string) ([]byte, error) { return os.ReadFile(path) }`)
	completion := newLocalizationRefinementCompletionForSymbols(
		symbol, []string{symbol, "storage/cache.go::Cache.Warm"})

	result, _, _, finalized := buildLocalizationExploreResultForTaskFinalized(completion, task, targets, 1600)
	envelope := decodeLocalizationEnvelope(t, result)

	if finalized.State == localizationStateLocalized || finalized.State == localizationStateAnswerReady {
		t.Fatalf("a page with a choice left to make released its read: %#v", finalized)
	}
	for _, evidence := range envelope.Evidence {
		if strings.TrimSpace(evidence.Source) != "" {
			t.Fatalf("a page that kept its read also paid for a body: %#v", evidence)
		}
	}
}

// The prescribed body is the only body a retiring page adds: a refinement that
// grew by every candidate's source would spend its whole budget restating what
// the one bounded read was going to fetch.
func TestARetiringPageAddsThePrescribedBodyAndNoOther(t *testing.T) {
	symbol, targets := prescribedBodyTargets(
		`func (d *DiskStorage) Load(path string) ([]byte, error) { return os.ReadFile(path) }`)
	envelope := localizationExploreEnvelope{
		Completion: newLocalizationRefinementCompletion(symbol),
		Evidence: []localizationEvidence{
			{Rank: 1, ID: symbol, Name: "DiskStorage.Load", Kind: "method", File: "storage/disk.go", Line: 12},
			{Rank: 2, ID: "storage/cache.go::Cache.Warm", Name: "Cache.Warm", Kind: "method", File: "storage/cache.go", Line: 40},
		},
	}
	packed := localizationEnvelopePackingPrescribedBody(envelope, targets, []int{0, 1}, symbol, 6400)

	var prescribed, others int
	for _, evidence := range packed.Evidence {
		if strings.TrimSpace(evidence.Source) == "" {
			continue
		}
		if evidence.ID == symbol {
			prescribed++
			continue
		}
		others++
	}
	if prescribed != 1 {
		t.Fatalf("prescribed body was not packed: %#v", packed.Evidence)
	}
	if others != 0 {
		t.Fatalf("packing added %d body/bodies beyond the prescribed one", others)
	}
}

// Inlining the body is only half the fix. A page that ships the body and still
// commands the read leaves the caller doing both, so the instruction has to go
// at the same moment the body arrives.
func TestAPageCarryingThePrescribedBodyStopsCommandingTheRead(t *testing.T) {
	const symbol = "repo/storage/disk.go::DiskStorage.Load"
	const task = "DiskStorage.Load returns an empty path when loading a stored record"
	body := "func (s *DiskStorage) Load(path string) ([]byte, error) { return s.read(path) }"

	for _, prescription := range []struct {
		name       string
		completion localizationCompletion
	}{
		{name: "exact read", completion: newLocalizationExactReadCompletion(symbol, false)},
		{name: "refinement", completion: newLocalizationRefinementCompletion(symbol)},
	} {
		t.Run(prescription.name, func(t *testing.T) {
			envelope := localizationExploreEnvelope{
				Completion: prescription.completion,
				Evidence: []localizationEvidence{
					firstCallEvidence(symbol, "DiskStorage.Load", "storage/disk.go", body),
				},
			}
			prescribed := localizationPrescribedSymbol(envelope.Completion)
			if prescribed != symbol {
				t.Fatalf("prescribed symbol = %q, want %q", prescribed, symbol)
			}
			if !localizationPrescribedReadSatisfiedByEnvelope(task, prescribed, envelope) {
				t.Fatal("a packed, aligned body is exactly what the prescribed read would return")
			}

			retired := localizationCompletionRetiringPrescribedRead(envelope.Completion)
			if retired.State != localizationStateAnswerReady {
				t.Fatalf("a satisfied prescription stayed non-terminal: %#v", retired)
			}
			if retired.RequiredAction != "respond" || retired.AllowedToolCalls != 0 {
				t.Fatalf("a satisfied prescription still prescribes a call: %#v", retired)
			}
			if strings.Contains(retired.Instruction, "read(") {
				t.Fatalf("a satisfied prescription still instructs a read: %q", retired.Instruction)
			}
			if retired.ExactSymbol != "" || retired.refinementSymbol != "" {
				t.Fatalf("a retired prescription still names a symbol to read: %#v", retired)
			}
		})
	}
}

// A body that is not there cannot retire anything, whichever state prescribed
// the read.
func TestAnAbsentBodyNeverRetiresThePrescription(t *testing.T) {
	const symbol = "repo/storage/disk.go::DiskStorage.Load"
	const task = "DiskStorage.Load returns an empty path when loading a stored record"
	envelope := localizationExploreEnvelope{
		Completion: newLocalizationRefinementCompletion(symbol),
		Evidence: []localizationEvidence{
			firstCallEvidence(symbol, "DiskStorage.Load", "storage/disk.go", ""),
		},
	}
	if localizationPrescribedReadSatisfiedByEnvelope(task, symbol, envelope) {
		t.Fatal("a signature-only row still owes the caller the body")
	}
}

// The read is retired only when its answer is genuinely in hand. A body that
// never fit the budget leaves the prescription standing.
func TestAnUnpackedPrescriptionStillAsksForTheRead(t *testing.T) {
	const task = "DiskStorage.Load returns an empty path when loading a stored record"
	symbol, targets := prescribedBodyTargets(strings.Repeat("x", 64_000))
	completion := newLocalizationRefinementCompletion(symbol)

	_, _, _, finalized := buildLocalizationExploreResultForTaskFinalized(completion, task, targets, 1000)
	if finalized.State == localizationStateAnswerReady {
		t.Fatalf("a prescription was retired without its body: %#v", finalized)
	}
}

// The server has already ranked and hydrated every refinement candidate. The
// model should not pay another full turn merely to choose and read the same
// preferred source. Alternate candidates remain visible as ranked evidence.
func TestBuildRefinementPacksOnlyThePreferredCandidate(t *testing.T) {
	const task = "DiskStorage.Load returns an empty path when loading a stored record"
	symbol, targets := prescribedBodyTargets(
		`func (d *DiskStorage) Load(path string) ([]byte, error) { return os.ReadFile(path) }`)
	alternate := targets[1].node.ID
	routes := map[string]localizationRefinementRoute{
		symbol:    {},
		alternate: {},
	}

	result, finalized, bounded, _ := buildLocalizationRefinementResultForTask(
		symbol, task, targets, 1600, routes)
	envelope := decodeLocalizationEnvelope(t, result)

	if finalized.State == localizationStateNeedsRefinement {
		t.Fatalf("the hydrated preferred refinement still requires a second call: completion=%#v evidence=%#v", finalized, envelope.Evidence)
	}
	if finalized.AllowedToolCalls != 0 {
		t.Fatalf("the one-shot refinement still permits another call: %#v", finalized)
	}
	if len(bounded) > 1 {
		t.Fatalf("more than the preferred route remained authorized: %#v", bounded)
	}
	if len(envelope.Evidence) < 2 || envelope.Evidence[1].ID != alternate {
		t.Fatalf("the alternate candidate disappeared from ranked evidence: %#v", envelope.Evidence)
	}
	var preferredBody, alternateBody bool
	for _, evidence := range envelope.Evidence {
		switch evidence.ID {
		case symbol:
			preferredBody = strings.TrimSpace(evidence.Source) != ""
		case alternate:
			alternateBody = strings.TrimSpace(evidence.Source) != ""
		}
	}
	if !preferredBody || alternateBody {
		t.Fatalf("body packing preferred=%t alternate=%t; want only preferred", preferredBody, alternateBody)
	}
}
