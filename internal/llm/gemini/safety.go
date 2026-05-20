package gemini

import "google.golang.org/genai"

// isSafetyFinish reports whether the candidate's finish_reason is one
// of the safety-blocked outcomes. These responses are NOT retryable
// and NOT "empty" in the sense the loop uses (they're deliberate
// refusals, not transient failures), so the loop must surface them
// rather than retrying or treating them as truncation.
//
// Distinct from the loop's StopRepairExhausted / StopError stops:
// safety means the model declined to answer for policy reasons; the
// caller should degrade gracefully (show a refusal message) rather
// than retry.
func isSafetyFinish(fr genai.FinishReason) bool {
	switch fr {
	case genai.FinishReasonSafety,
		genai.FinishReasonProhibitedContent,
		genai.FinishReasonBlocklist,
		genai.FinishReasonSPII,
		genai.FinishReasonImageSafety,
		genai.FinishReasonImageProhibitedContent,
		genai.FinishReasonRecitation,
		genai.FinishReasonImageRecitation:
		return true
	}
	return false
}
