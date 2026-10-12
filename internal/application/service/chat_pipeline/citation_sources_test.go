package chatpipeline

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestExpandedContextHasSeparateCitationBodies(t *testing.T) {
	a := &types.Chunk{
		ID:              "a",
		KnowledgeID:     "doc",
		KnowledgeBaseID: "kb",
		Content:         "Opening greeting.",
		ParentChunkID:   "p",
		ChunkType:       types.ChunkTypeText,
		IsEnabled:       true,
	}
	b := &types.Chunk{
		ID:              "b",
		KnowledgeID:     "doc",
		KnowledgeBaseID: "kb",
		Content:         "Exact later quotation.",
		ParentChunkID:   "p",
		ChunkType:       types.ChunkTypeText,
		IsEnabled:       true,
		ChunkIndex:      1,
	}
	disabled := &types.Chunk{
		ID:              "disabled",
		KnowledgeID:     "doc",
		KnowledgeBaseID: "kb",
		Content:         "Exact later quotation.",
		ParentChunkID:   "p",
		ChunkType:       types.ChunkTypeText,
	}
	foreign := &types.Chunk{
		ID:              "foreign",
		KnowledgeID:     "other",
		KnowledgeBaseID: "kb",
		Content:         "Opening greeting.",
		IsEnabled:       true,
	}
	repo := &expandChunkRepo{
		chunks: map[string]*types.Chunk{
			"a": a,
			"p": {
				ID:        "p",
				ChunkType: types.ChunkTypeParentText,
			},
		},
		children: map[string][]*types.Chunk{"p": {
			a,
			b,
			disabled,
			foreign,
		}},
	}
	p := &PluginMerge{chunkRepo: repo}
	merged := &types.SearchResult{
		ID:               "a",
		KnowledgeID:      "doc",
		KnowledgeBaseID:  "kb",
		Content:          "Opening greeting.\n\nExact later quotation.",
		ContentRewritten: true,
	}
	p.attachCitationSources(context.Background(), []*types.SearchResult{merged})
	got := expandCitationSources([]*types.SearchResult{merged, merged})
	require.Len(t, got, 2)
	require.Equal(t, "a", got[0].ID)
	require.Equal(t, a.Content, got[0].Content)
	require.Equal(t, "b", got[1].ID)
	require.Equal(t, b.Content, got[1].Content)
	require.Empty(t, got[0].SubChunkID)
}

func TestExpandedEvidenceFailsClosedAndPreservesPlainSources(t *testing.T) {
	got := expandCitationSources([]*types.SearchResult{
		{
			ID:               "expanded",
			Content:          "parent text under child ID",
			ContentRewritten: true,
		},
		{
			ID:      "plain",
			Content: "actual source",
		},
	})
	require.Len(t, got, 1)
	require.Equal(t, "plain", got[0].ID)
}

func TestModelHandlesReferenceTheActualExpandedChild(t *testing.T) {
	a := &types.SearchResult{
		ID:              "opening",
		KnowledgeID:     "doc",
		KnowledgeBaseID: "kb",
		KnowledgeTitle:  "Report",
		Content:         "Opening greeting.",
	}
	b := &types.SearchResult{
		ID:              "later",
		KnowledgeID:     "doc",
		KnowledgeBaseID: "kb",
		KnowledgeTitle:  "Report",
		ChunkIndex:      1,
		Content:         "Exact later quotation.",
	}
	rendered := "old expanded context"
	manage := &types.ChatManage{
		PipelineRequest: types.PipelineRequest{
			Query:         "question",
			SummaryConfig: types.SummaryConfig{Prompt: "system"},
		},
		PipelineState: types.PipelineState{
			RenderedContexts: rendered,
			UserContent:      rendered,
			MergeResult: []*types.SearchResult{{
				ID:               "opening",
				ContentRewritten: true,
				CitationSources: []*types.SearchResult{
					a,
					b,
				},
			}},
		},
	}
	messages, registry := prepareMessagesWithModelContext(context.Background(), manage)
	require.Contains(t, messages[1].Content, `<chunk id="c2"`)
	require.Contains(t, registry.DecodeOutputText(`<ref id="c2"/>`), `chunk_id="later"`)
	require.Contains(t, registry.DecodeOutputText(`<ref id="c1"/>`), `chunk_id="opening"`)
	manage.MergeResult[0].CitationSources = nil
	messages, _ = prepareMessagesWithModelContext(context.Background(), manage)
	require.NotContains(t, messages[1].Content, rendered)
	require.Contains(t, messages[1].Content, "could not be verified")
}

// A merged text entry carries its image captions only on the merge result;
// the per-chunk citation sources are rebuilt from stored text chunks whose
// image_info column is empty. The prompt-time chat enrichment reads the
// source's ImageInfo, so a source that drops the parent's collection loses
// every caption next to its image refs.
func TestExpandCitationSources_TextSourceKeepsCaptionEnrichment(t *testing.T) {
	imageInfo, err := json.Marshal([]types.ImageInfo{
		{URL: "resource://img-1", Caption: "流程图：三步审批"},
	})
	if err != nil {
		t.Fatal(err)
	}
	repo := &expandChunkRepo{
		chunks: map[string]*types.Chunk{
			"t1": {
				ID: "t1", ParentChunkID: "p1", ChunkType: types.ChunkTypeText, ChunkIndex: 5,
				KnowledgeID: "doc", KnowledgeBaseID: "kb", IsEnabled: true,
				Content: "第二步 复核\n\n![demo](resource://img-1)",
			},
			"p1": {
				ID: "p1", ChunkType: types.ChunkTypeParentText, ChunkIndex: 4,
				KnowledgeID: "doc", KnowledgeBaseID: "kb", IsEnabled: true,
				Content: "第一步 提交\n\n第二步 复核\n\n![demo](resource://img-1)",
			},
		},
	}
	plugin := &PluginMerge{chunkRepo: repo}
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(1))
	result := &types.SearchResult{
		ID: "merged-1", KnowledgeID: "doc", KnowledgeBaseID: "kb",
		ChunkType:  string(types.ChunkTypeText),
		Content:    "第二步 复核\n\n![demo](resource://img-1)",
		ImageInfo:  string(imageInfo),
		SubChunkID: []string{"t1"},
	}

	plugin.attachCitationSources(ctx, []*types.SearchResult{result})
	if len(result.CitationSources) != 1 {
		t.Fatalf("citation sources = %d, want 1", len(result.CitationSources))
	}
	if result.CitationSources[0].ImageInfo == "" {
		t.Fatalf("text citation source dropped the merge result's image info")
	}

	expanded := expandCitationSources([]*types.SearchResult{result})
	if len(expanded) != 1 {
		t.Fatalf("expanded results = %d, want 1", len(expanded))
	}
	passage := getEnrichedPassageForChat(ctx, expanded[0])
	if !strings.Contains(passage, "> **Image caption:** 流程图：三步审批") {
		t.Fatalf("caption was not injected next to the image ref: %q", passage)
	}
}

func TestExpandCitationSources_ImageSourceKeepsOwnImageInfo(t *testing.T) {
	parentInfo, err := json.Marshal([]types.ImageInfo{
		{URL: "resource://img-1", Caption: "父条目封面"},
	})
	if err != nil {
		t.Fatal(err)
	}
	childInfo, err := json.Marshal([]types.ImageInfo{
		{URL: "resource://img-1", Caption: "子图自己的题注"},
	})
	if err != nil {
		t.Fatal(err)
	}
	repo := &expandChunkRepo{
		chunks: map[string]*types.Chunk{
			"t1": {
				ID: "t1", ParentChunkID: "p1", ChunkType: types.ChunkTypeText, ChunkIndex: 5,
				KnowledgeID: "doc", KnowledgeBaseID: "kb", IsEnabled: true,
				Content: "正文\n\n![demo](resource://img-1)",
			},
			"p1": {
				ID: "p1", ChunkType: types.ChunkTypeParentText,
				KnowledgeID: "doc", KnowledgeBaseID: "kb", IsEnabled: true,
				Content: "正文",
			},
		},
		children: map[string][]*types.Chunk{
			"t1": {
				{
					ID: "cap-1", ParentChunkID: "t1", ChunkType: types.ChunkTypeImageCaption,
					KnowledgeID: "doc", KnowledgeBaseID: "kb", IsEnabled: true,
					Content: "子图自己的题注", ImageInfo: string(childInfo),
				},
			},
		},
	}
	plugin := &PluginMerge{chunkRepo: repo}
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(1))
	result := &types.SearchResult{
		ID: "merged-1", KnowledgeID: "doc", KnowledgeBaseID: "kb",
		ChunkType:  string(types.ChunkTypeText),
		Content:    "正文\n\n![demo](resource://img-1)",
		ImageInfo:  string(parentInfo),
		SubChunkID: []string{"t1"},
	}

	plugin.attachCitationSources(ctx, []*types.SearchResult{result})
	if len(result.CitationSources) != 2 {
		t.Fatalf("citation sources = %d, want 2", len(result.CitationSources))
	}
	for _, source := range result.CitationSources {
		if source.ID == "cap-1" && source.ImageInfo != string(childInfo) {
			t.Fatalf("image caption source lost its own image info: %q", source.ImageInfo)
		}
	}
}
