package cli

import (
	"fmt"
	"strings"
)

// splitModel lowercases a model name and splits off its provider (the text
// before the last "/", if any). The model part has "." replaced with "-", so
// "claude-haiku-4.5" and "claude-haiku-4-5" compare equal. Both operations
// keep byte offsets for ASCII names, so an index into the model part also
// indexes the original name's model part.
func splitModel(name string) (provider, model string) {
	name = strings.ToLower(name)
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		provider, name = name[:i], name[i+1:]
	}
	return provider, strings.ReplaceAll(name, ".", "-")
}

// modelMatches is check's prefix rule: the served model is the expected one,
// or a more specific version of it. Providers must agree when both sides name
// one. The served name must equal the expected name, or start with it followed
// by one of "-", "_" or ":", so "gpt-5" matches "gpt-5.6-luna" but not
// "gpt-50", and "sonnet" does not match "claude-sonnet-5-5".
func modelMatches(expected, served string) bool {
	ep, em := splitModel(expected)
	sp, sm := splitModel(served)
	if ep != "" && sp != "" && ep != sp {
		return false
	}
	if sm == em {
		return true
	}
	if !strings.HasPrefix(sm, em) || len(sm) == len(em) {
		return false
	}
	return strings.ContainsRune(modelSeps, rune(sm[len(em)]))
}

// modelSeps may follow the expected name in a more specific served name. "/"
// cannot appear: splitModel has already split at the last one.
const modelSeps = "-_:"

// validExpectation rejects an expectation with no model part ("p/", "/", "").
func validExpectation(expected string) error {
	if _, m := splitModel(expected); m == "" {
		return usagef("expected model %q has no model name after the provider", expected)
	}
	return nil
}

// modelMismatchHint explains a failed match. When the served name contains the
// expected name somewhere after its start (a short alias such as "haiku" for
// "claude-haiku-4-5"), it suggests the name up to and including the match.
func modelMismatchHint(expected, served string) string {
	const substituted = "the backend substituted a different model"
	ep, em := splitModel(expected)
	sp, sm := splitModel(served)
	if em != "" && strings.ContainsRune(modelSeps, rune(em[len(em)-1])) {
		return fmt.Sprintf("drop the trailing separator from the expected name (%s)", strings.TrimRight(expected, modelSeps+"."))
	}
	if ep != "" && sp != "" && ep != sp {
		if strings.HasSuffix(sp, "/"+ep) {
			full := served[:strings.LastIndexByte(served, '/')]
			return fmt.Sprintf("the served provider is %s; include it in --expect-model", full)
		}
		return substituted
	}
	i := strings.Index(sm, em)
	if em == "" || i <= 0 {
		return substituted
	}
	// Slice the served name as reported, keeping its case and dots.
	orig := served
	if j := strings.LastIndexByte(orig, '/'); j >= 0 {
		orig = orig[j+1:]
	}
	if len(orig) != len(sm) {
		orig = sm // non-ASCII case folding changed the length
	}
	suggest := orig[:i+len(em)]
	if j := strings.LastIndexByte(expected, '/'); j >= 0 {
		suggest = expected[:j+1] + suggest
	}
	return fmt.Sprintf("the served name contains %q but does not start with it: name the model from its start, e.g. --model %s, or keep --model and pass --expect-model %s", em, suggest, suggest)
}

// clientNote marks a pass whose model the agent CLI recorded itself.
const clientNote = " (client-reported: the backend does not confirm the serving model, so a provider-side swap would not be detected)"

// modelStep checks the served model against the expectation. flag names where
// the expectation came from ("--model" or "--expect-model"; "" when none was
// given). served and source are the parser's model and model_source fields.
// A client-reported model still gets the prefix rule (that catches an agent
// CLI resolving a pattern to another model), but a pass says it is unconfirmed.
func modelStep(expected, flag, served, source string) checkStep {
	st := checkStep{Name: "model"}
	verb := "requested"
	if flag == "--expect-model" {
		verb = "expected"
	}
	switch {
	case served == "" && expected == "":
		st.Status, st.Detail = stepSkip, "backend does not report its model"
	case served == "":
		st.Status, st.Detail = stepSkip, fmt.Sprintf("backend does not report its model, so %s %s could not be verified", verb, expected)
	case expected == "":
		st.Status, st.Detail = stepPass, "served "+served
	case modelMatches(expected, served):
		st.Status, st.Detail = stepPass, fmt.Sprintf("%s %s, served %s", verb, expected, served)
	default:
		st.Status = stepFail
		st.Detail = fmt.Sprintf("%s %s, but the backend served %s", verb, expected, served)
		st.Hint = modelMismatchHint(expected, served)
	}
	if st.Status == stepPass && source == "client" {
		st.Detail += clientNote
	}
	return st
}
