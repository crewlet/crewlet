package embeddings

import (
	"context"
	"fmt"
)

// EmbedWholeBatch is [EmbedWhole] for many texts in one call: every text is
// [Chunks]ed at the embedder's own bound, every piece of every text goes into
// ONE [BatchEmbedder.EmbedBatch] call — packed into requests by the model's
// limits — and each text's pieces are [Pool]ed into its vector.
//
// It exists for the caller with many short texts and the occasional long one:
// a seat's diary, re-embedded after a model change, is hundreds of notes that
// each fit one input, and sending them one Embed at a time is hundreds of round
// trips for what one request carries. A note longer than the model's input —
// an operator-stated window smaller than the note bound — is represented
// whole, exactly as EmbedWhole would represent it, rather than refusing the
// batch it is in.
//
// ONE-TO-ONE WITH THE INPUT, positionally, like EmbedBatch: a text with
// nothing to embed keeps its slot as a nil vector. A text of one piece takes
// that piece's vector as the provider answered it, unpooled — so it is the
// vector EmbedBatch gives the same text, which is the vector Embed gives it
// wherever the provider answers an array as it answers a string. A failure
// fails the call, and no vector the call already paid for is returned beside
// the error.
func EmbedWholeBatch(ctx context.Context, e BatchEmbedder, texts []string) ([][]float32, error) {
	bound := e.Limits().InputBytes
	var (
		pieces []string
		// spans[i] is text i's pieces in the flat batch: [start, end).
		spans = make([][2]int, len(texts))
	)
	for i, text := range texts {
		chunks := Chunks(text, bound)
		spans[i] = [2]int{len(pieces), len(pieces) + len(chunks)}
		pieces = append(pieces, chunks...)
	}
	out := make([][]float32, len(texts))
	if len(pieces) == 0 {
		return out, nil
	}
	vectors, err := e.EmbedBatch(ctx, pieces)
	if err != nil {
		return nil, err
	}
	if len(vectors) != len(pieces) {
		return nil, fmt.Errorf("embeddings: %s answered %d vectors for %d pieces",
			e.Model(), len(vectors), len(pieces))
	}
	for i, span := range spans {
		start, end := span[0], span[1]
		switch end - start {
		case 0:
			continue
		case 1:
			out[i] = vectors[start]
			continue
		}
		weights := make([]int, 0, end-start)
		for _, piece := range pieces[start:end] {
			weights = append(weights, len(piece))
		}
		pooled, err := Pool(vectors[start:end], weights, e.Width())
		if err != nil {
			return nil, fmt.Errorf("embeddings: text %d: %w", i, err)
		}
		out[i] = pooled
	}
	return out, nil
}
