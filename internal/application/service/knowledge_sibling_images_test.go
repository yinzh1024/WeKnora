package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/infrastructure/docparser"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// fakeSiblingRepo emulates the folder-exact + keyword-substring behaviour of
// ListPagedKnowledgeByKnowledgeBaseID over a fixed row set.
type fakeSiblingRepo struct {
	interfaces.KnowledgeRepository
	rows    []*types.Knowledge
	err     error
	queried []types.KnowledgeListFilter
}

func (f *fakeSiblingRepo) ListPagedKnowledgeByKnowledgeBaseID(
	_ context.Context,
	_ uint64,
	_ string,
	page *types.Pagination,
	filter types.KnowledgeListFilter,
) ([]*types.Knowledge, int64, error) {
	f.queried = append(f.queried, filter)
	if f.err != nil {
		return nil, 0, f.err
	}
	keyword := strings.ToLower(filter.Keyword)
	var matched []*types.Knowledge
	for _, row := range f.rows {
		if filter.FolderScope == types.FolderScopeExact && row.FolderPath != filter.FolderPath {
			continue
		}
		if keyword != "" &&
			!strings.Contains(strings.ToLower(row.FileName), keyword) &&
			!strings.Contains(strings.ToLower(row.Title), keyword) {
			continue
		}
		matched = append(matched, row)
	}
	total := int64(len(matched))
	start := (page.GetPage() - 1) * page.GetPageSize()
	if start >= len(matched) {
		return nil, total, nil
	}
	end := start + page.GetPageSize()
	if end > len(matched) {
		end = len(matched)
	}
	return matched[start:end], total, nil
}

// fakeSiblingFileSvc serves preloaded originals and records SaveBytes calls.
type fakeSiblingFileSvc struct {
	interfaces.FileService
	files map[string][]byte
	saved []string
}

func (f *fakeSiblingFileSvc) GetFile(_ context.Context, filePath string) (io.ReadCloser, error) {
	data, ok := f.files[filePath]
	if !ok {
		return nil, fmt.Errorf("not found: %s", filePath)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (f *fakeSiblingFileSvc) SaveBytes(_ context.Context, data []byte, _ uint64, fileName string, _ bool) (string, error) {
	f.saved = append(f.saved, fileName)
	return "local://test/" + fileName, nil
}

func TestSiblingImageLocation(t *testing.T) {
	tests := []struct {
		name     string
		base     string
		ref      string
		wantFold string
		wantName string
		wantOK   bool
	}{
		{"root folder", "", "images/foo.png", "images", "foo.png", true},
		{"dot prefix", "notes", "./images/foo.png", "notes/images", "foo.png", true},
		{"one level up", "notes/chapter", "../images/foo.png", "notes/images", "foo.png", true},
		{"windows separators", "notes", `images\foo.png`, "notes/images", "foo.png", true},
		{"query and percent encoding", "notes", "images/my%20photo.jpg?raw=1", "notes/images", "my photo.jpg", true},
		{"sibling next to root document", "", "photo.jpg", "", "photo.jpg", true},
		{"escape above root rejected", "", "../../secret.png", "", "", false},
		{"absolute path rejected", "", "/abs.png", "", "", false},
		{"scheme rejected", "", "https://cdn/x.png", "", "", false},
		{"fragment only rejected", "", "#anchor", "", "", false},
		{"empty rejected", "", "   ", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			folder, name, ok := siblingImageLocation(tt.base, tt.ref)
			if ok != tt.wantOK || folder != tt.wantFold || name != tt.wantName {
				t.Errorf("siblingImageLocation(%q, %q) = (%q, %q, %v), want (%q, %q, %v)",
					tt.base, tt.ref, folder, name, ok, tt.wantFold, tt.wantName, tt.wantOK)
			}
		})
	}
}

func TestInjectSiblingImageRefsInjectsMatchedSiblingsOnly(t *testing.T) {
	knowledge := &types.Knowledge{
		ID:              "doc-1",
		TenantID:        7,
		KnowledgeBaseID: "kb-1",
		FolderPath:      "notes",
		FileName:        "page.md",
	}
	repo := &fakeSiblingRepo{rows: []*types.Knowledge{
		{ID: "doc-1", TenantID: 7, KnowledgeBaseID: "kb-1", FolderPath: "notes/images", FileName: "self.png", FilePath: "local://orig/self.png"},
		{ID: "s-foo", TenantID: 7, KnowledgeBaseID: "kb-1", FolderPath: "notes/images", FileName: "foo.png", FilePath: "local://orig/foo.png"},
		{ID: "s-bar", TenantID: 7, KnowledgeBaseID: "kb-1", FolderPath: "notes/images", FileName: "bar.jpg", FilePath: "local://orig/bar.jpg"},
		{ID: "s-other", TenantID: 7, KnowledgeBaseID: "kb-1", FolderPath: "elsewhere", FileName: "foo.png", FilePath: "local://orig/other.png"},
		{ID: "s-noPath", TenantID: 7, KnowledgeBaseID: "kb-1", FolderPath: "notes/images", FileName: "nopath.png"},
	}}
	fileSvc := &fakeSiblingFileSvc{files: map[string][]byte{
		"local://orig/foo.png": []byte("\x89PNG\r\n\x1a\nfoo"),
		"local://orig/bar.jpg": []byte("\xff\xd8\xff\xe0bar"),
	}}
	result := &types.ReadResult{MarkdownContent: strings.Join([]string{
		"![a](images/foo.png)",
		`![b](images/foo.png "标题")`,
		`<img src="images/bar.jpg" alt="x">`,
		"![missing](images/missing.png)",
		"![self](images/self.png)",
		"![noPath](images/nopath.png)",
		"![scheme](https://cdn/x.png)",
	}, "\n")}

	s := &knowledgeService{repo: repo}
	s.injectSiblingImageRefs(context.Background(), knowledge, fileSvc, result)

	if len(result.ImageRefs) != 2 {
		t.Fatalf("ImageRefs = %#v, want exactly foo.png + bar.jpg", result.ImageRefs)
	}
	byRef := map[string]types.ImageRef{}
	for _, ref := range result.ImageRefs {
		byRef[ref.OriginalRef] = ref
	}
	if _, ok := byRef["images/foo.png"]; !ok {
		t.Errorf("missing injected ref for images/foo.png: %#v", byRef)
	}
	if _, ok := byRef["images/bar.jpg"]; !ok {
		t.Errorf("missing injected ref for images/bar.jpg: %#v", byRef)
	}
	if foo := byRef["images/foo.png"]; foo.Filename != "foo.png" || foo.MimeType != "image/png" || !foo.IsOriginal {
		t.Errorf("foo.png ref = %#v", foo)
	}
	if bar := byRef["images/bar.jpg"]; bar.MimeType != "image/jpeg" {
		t.Errorf("bar.jpg ref = %#v", bar)
	}
	// The folder-exact filter must be passed down for every lookup.
	for i, q := range repo.queried {
		if q.FolderScope != types.FolderScopeExact {
			t.Errorf("query[%d] FolderScope = %v, want exact", i, q.FolderScope)
		}
	}
}

// TestInjectThenResolveAndStoreRewritesReferences runs the injection through
// the real resolver: injected bytes must flow into the generic pipeline and
// come out as stored ServingURLs in the markdown.
func TestInjectThenResolveAndStoreRewritesReferences(t *testing.T) {
	knowledge := &types.Knowledge{
		ID:              "doc-1",
		TenantID:        7,
		KnowledgeBaseID: "kb-1",
		FolderPath:      "",
		FileName:        "page.md",
	}
	repo := &fakeSiblingRepo{rows: []*types.Knowledge{
		{ID: "s-foo", TenantID: 7, KnowledgeBaseID: "kb-1", FolderPath: "images", FileName: "foo.png", FilePath: "local://orig/foo.png"},
		{ID: "s-bar", TenantID: 7, KnowledgeBaseID: "kb-1", FolderPath: "images", FileName: "bar.jpg", FilePath: "local://orig/bar.jpg"},
	}}
	fileSvc := &fakeSiblingFileSvc{files: map[string][]byte{
		"local://orig/foo.png": []byte("\x89PNG\r\n\x1a\nfoo"),
		"local://orig/bar.jpg": []byte("\xff\xd8\xff\xe0bar"),
	}}
	result := &types.ReadResult{MarkdownContent: "![a](images/foo.png)\n\n<img src=\"images/bar.jpg\">"}

	s := &knowledgeService{repo: repo}
	s.injectSiblingImageRefs(context.Background(), knowledge, fileSvc, result)

	out, images, err := docparser.NewImageResolver().ResolveAndStore(context.Background(), result, fileSvc, 7)
	if err != nil {
		t.Fatalf("ResolveAndStore: %v", err)
	}
	if strings.Contains(out, "images/foo.png") || strings.Contains(out, "images/bar.jpg") {
		t.Errorf("relative refs survived resolution: %s", out)
	}
	if !strings.Contains(out, "local://test/") {
		t.Errorf("expected stored serving URLs in output: %s", out)
	}
	if len(images) != 2 {
		t.Errorf("stored images = %d, want 2 (md + html): %#v", len(images), images)
	}
	if len(fileSvc.saved) != 2 {
		t.Errorf("SaveBytes calls = %d, want 2", len(fileSvc.saved))
	}
}

func TestInjectSiblingImageRefsToleratesRepoErrors(t *testing.T) {
	repo := &fakeSiblingRepo{err: fmt.Errorf("db down")}
	fileSvc := &fakeSiblingFileSvc{files: map[string][]byte{}}
	result := &types.ReadResult{MarkdownContent: "![a](images/foo.png)"}

	s := &knowledgeService{repo: repo}
	s.injectSiblingImageRefs(context.Background(), &types.Knowledge{ID: "d", KnowledgeBaseID: "kb"}, fileSvc, result)

	if len(result.ImageRefs) != 0 {
		t.Errorf("ImageRefs = %#v, want none", result.ImageRefs)
	}
}
