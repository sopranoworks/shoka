package librariansrc

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sopranoworks/shoka/pkg/librarian/classifier"
)

const (
	chunkMinChars     = 50
	chunkMaxChars     = 500
	chunkMinSimilarity = 0.3
)

// ChunkEmbedder embeds text into a vector for chunk-level similarity filtering.
type ChunkEmbedder interface {
	EmbedText(ctx context.Context, text string) ([]float64, error)
}

type chunk struct {
	content       string
	startLine     int // 0-based inclusive
	endLine       int // 0-based exclusive
	isFrontmatter bool
}

// splitChunks splits file content into chunks for similarity filtering.
//
// Frontmatter (YAML header delimited by "---") is a separate chunk, always
// included. Remaining content is split on empty-line paragraph boundaries.
// Small chunks (< minChars) are merged with the next; large chunks
// (> maxChars) are split on single newlines.
func splitChunks(content string, minChars, maxChars int) []chunk {
	if content == "" {
		return nil
	}
	lines := strings.Split(content, "\n")

	var fm *chunk
	bodyStart := 0
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" {
		for i := 1; i < len(lines); i++ {
			if strings.TrimSpace(lines[i]) == "---" {
				fm = &chunk{
					content:       strings.Join(lines[:i+1], "\n"),
					startLine:     0,
					endLine:       i + 1,
					isFrontmatter: true,
				}
				bodyStart = i + 1
				break
			}
		}
	}

	// Group remaining lines into paragraphs separated by blank lines.
	var raw []chunk
	var paraLines []string
	paraStart := bodyStart
	for i := bodyStart; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "" {
			if len(paraLines) > 0 {
				raw = append(raw, chunk{
					content:   strings.Join(paraLines, "\n"),
					startLine: paraStart,
					endLine:   paraStart + len(paraLines),
				})
				paraLines = nil
			}
			paraStart = i + 1
		} else {
			if len(paraLines) == 0 {
				paraStart = i
			}
			paraLines = append(paraLines, lines[i])
		}
	}
	if len(paraLines) > 0 {
		raw = append(raw, chunk{
			content:   strings.Join(paraLines, "\n"),
			startLine: paraStart,
			endLine:   paraStart + len(paraLines),
		})
	}

	// Split oversized chunks on single newlines.
	var sized []chunk
	for _, c := range raw {
		if len(c.content) > maxChars {
			cLines := lines[c.startLine:c.endLine]
			sized = append(sized, splitLargeChunk(cLines, c.startLine, maxChars)...)
		} else {
			sized = append(sized, c)
		}
	}

	merged := mergeSmallChunks(sized, minChars)

	if fm != nil {
		merged = append([]chunk{*fm}, merged...)
	}
	return merged
}

func splitLargeChunk(cLines []string, absStart, maxChars int) []chunk {
	var result []chunk
	var cur []string
	curStart := absStart
	curLen := 0

	for i, line := range cLines {
		ll := len(line)
		if curLen > 0 && curLen+1+ll > maxChars {
			result = append(result, chunk{
				content:   strings.Join(cur, "\n"),
				startLine: curStart,
				endLine:   absStart + i,
			})
			cur = nil
			curLen = 0
			curStart = absStart + i
		}
		cur = append(cur, line)
		if curLen > 0 {
			curLen++
		}
		curLen += ll
	}
	if len(cur) > 0 {
		result = append(result, chunk{
			content:   strings.Join(cur, "\n"),
			startLine: curStart,
			endLine:   absStart + len(cLines),
		})
	}
	return result
}

func mergeSmallChunks(chunks []chunk, minChars int) []chunk {
	if len(chunks) <= 1 {
		return chunks
	}
	var result []chunk
	for i := 0; i < len(chunks); i++ {
		c := chunks[i]
		for len(c.content) < minChars && i+1 < len(chunks) {
			i++
			c.content += "\n" + chunks[i].content
			c.endLine = chunks[i].endLine
		}
		result = append(result, c)
	}
	return result
}

// formatFilteredChunks concatenates kept chunks with position markers.
func formatFilteredChunks(chunks []chunk) string {
	var b strings.Builder
	for i, c := range chunks {
		if i > 0 {
			b.WriteByte('\n')
		}
		if c.isFrontmatter {
			b.WriteString(c.content)
		} else {
			fmt.Fprintf(&b, "[lines %d-%d]\n%s", c.startLine+1, c.endLine, c.content)
		}
	}
	return b.String()
}

// chunkFilter splits content into chunks, embeds each against the query, and
// returns only chunks above the similarity threshold. Frontmatter is always
// included. If no body chunks pass the threshold, the first body chunk is kept
// so the LLM has some context.
func chunkFilter(ctx context.Context, embedder ChunkEmbedder, query, content string, log *slog.Logger) (string, error) {
	chunks := splitChunks(content, chunkMinChars, chunkMaxChars)
	if len(chunks) <= 1 {
		return content, nil
	}

	queryVec, err := embedder.EmbedText(ctx, query)
	if err != nil {
		return "", fmt.Errorf("embed query: %w", err)
	}

	type scored struct {
		ch  chunk
		sim float64
	}

	var body []scored
	var fm []chunk
	embedStart := time.Now()

	for _, ch := range chunks {
		if ch.isFrontmatter {
			fm = append(fm, ch)
			continue
		}
		vec, err := embedder.EmbedText(ctx, ch.content)
		if err != nil {
			return "", fmt.Errorf("embed chunk [lines %d-%d]: %w", ch.startLine+1, ch.endLine, err)
		}
		sim := classifier.CosineSimilarity(queryVec, vec)
		body = append(body, scored{ch: ch, sim: sim})
	}
	embedMs := time.Since(embedStart).Milliseconds()

	var kept []chunk
	belowThreshold := 0
	for _, s := range body {
		if s.sim >= chunkMinSimilarity {
			kept = append(kept, s.ch)
		} else {
			belowThreshold++
		}
	}

	if len(kept) == 0 && len(body) > 0 {
		kept = append(kept, body[0].ch)
	}

	all := append(fm, kept...)
	filteredBytes := 0
	for _, c := range all {
		filteredBytes += len(c.content)
	}

	log.Debug("chunk filter",
		slog.Int("total_chunks", len(chunks)),
		slog.Int("embedded_chunks", len(body)),
		slog.Int("above_threshold", len(kept)),
		slog.Int("below_threshold", belowThreshold),
		slog.Int64("embed_ms", embedMs),
		slog.Int("original_bytes", len(content)),
		slog.Int("filtered_bytes", filteredBytes),
		slog.String("filtered_pct", fmt.Sprintf("%.0f%%",
			100*float64(filteredBytes)/float64(max(len(content), 1)))))

	return formatFilteredChunks(all), nil
}
