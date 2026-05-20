package gemini

import (
	"google.golang.org/genai"

	"github.com/zenocy/zeno-v2/internal/llm"
)

// extractCitations turns Gemini's groundingMetadata into the provider-
// agnostic []llm.Citation surface. Two shapes coexist on the wire:
//
//  1. Each GroundingSupport carries indices into GroundingChunks plus
//     a Segment marking the supported span in the response text. This
//     is the rich case — yields one Citation per (chunk × support) pair
//     with byte offsets.
//
//  2. The model only attaches a flat GroundingChunks list (no
//     supports). Yields one Citation per chunk, with zero offsets so
//     downstream surfaces know it applies to the whole response.
//
// In both cases only web chunks are surfaced; map/image/retrieved-
// context chunks are not yet wired through the trace UI.
func extractCitations(meta *genai.GroundingMetadata) []llm.Citation {
	if meta == nil {
		return nil
	}
	if len(meta.GroundingSupports) > 0 {
		return citationsFromSupports(meta)
	}
	return citationsFromChunks(meta.GroundingChunks)
}

func citationsFromSupports(meta *genai.GroundingMetadata) []llm.Citation {
	var out []llm.Citation
	for _, sup := range meta.GroundingSupports {
		if sup == nil {
			continue
		}
		seg := sup.Segment
		for _, idx := range sup.GroundingChunkIndices {
			if int(idx) < 0 || int(idx) >= len(meta.GroundingChunks) {
				continue
			}
			chunk := meta.GroundingChunks[idx]
			c, ok := citationFromChunk(chunk)
			if !ok {
				continue
			}
			if seg != nil {
				c.StartIndex = int(seg.StartIndex)
				c.EndIndex = int(seg.EndIndex)
			}
			out = append(out, c)
		}
	}
	return out
}

func citationsFromChunks(chunks []*genai.GroundingChunk) []llm.Citation {
	var out []llm.Citation
	for _, ch := range chunks {
		c, ok := citationFromChunk(ch)
		if !ok {
			continue
		}
		out = append(out, c)
	}
	return out
}

func citationFromChunk(ch *genai.GroundingChunk) (llm.Citation, bool) {
	if ch == nil || ch.Web == nil {
		return llm.Citation{}, false
	}
	if ch.Web.URI == "" {
		return llm.Citation{}, false
	}
	return llm.Citation{
		Title: ch.Web.Title,
		URI:   ch.Web.URI,
	}, true
}
