package llm

import "context"

// tiered sends heavy requests to one provider and routine ones to another, so the background
// work (tagging, facts, planning, judging) can use its own provider and its own limits.
type tiered struct {
	heavy, routine Provider
}

// Name is both providers' names.
func (t tiered) Name() string { return t.heavy.Name() + "+" + t.routine.Name() }

// Locality is cloud when either provider is, since masking is decided once for both.
func (t tiered) Locality() Locality {
	if t.heavy.Locality() == Cloud || t.routine.Locality() == Cloud {
		return Cloud
	}
	return Local
}

// ContextTokens is the smaller of the two.
func (t tiered) ContextTokens() int { return min(t.heavy.ContextTokens(), t.routine.ContextTokens()) }

// CountTokens is the larger of the two counts, so a request fits either.
func (t tiered) CountTokens(text string) int {
	return max(t.heavy.CountTokens(text), t.routine.CountTokens(text))
}

// Complete asks the provider for the request's tier.
func (t tiered) Complete(ctx context.Context, r Request) (Completion, error) {
	if r.Tier == Routine {
		return t.routine.Complete(ctx, r)
	}
	return t.heavy.Complete(ctx, r)
}
