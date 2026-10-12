package docparser

import (
	"reflect"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestCollectRelativeImageRefs(t *testing.T) {
	tests := []struct {
		name string
		md   string
		want []string
	}{
		{
			name: "plain relative markdown image",
			md:   "before ![chart](images/foo.png) after",
			want: []string{"images/foo.png"},
		},
		{
			name: "dot prefixed and nested paths",
			md:   "![a](./images/foo.png) ![b](../up/bar.jpg) ![c](assets/deep/baz.webp)",
			want: []string{"./images/foo.png", "../up/bar.jpg", "assets/deep/baz.webp"},
		},
		{
			name: "windows separators stay verbatim",
			md:   `![a](images\foo.png)`,
			want: []string{`images\foo.png`},
		},
		{
			name: "title suffix is stripped",
			md:   `![a](images/foo.png "标题") ![b](images/bar.png 'title')`,
			want: []string{"images/foo.png", "images/bar.png"},
		},
		{
			name: "angle bracket destination",
			md:   "![a](<images/my foo.png>)",
			want: []string{"images/my foo.png"},
		},
		{
			name: "html img src",
			md:   `<p><img class="shot" src="  images/profile.jpg  " alt="x"></p>`,
			want: []string{"images/profile.jpg"},
		},
		{
			name: "markdown and html duplicates collapse",
			md:   "![a](images/foo.png) <img src=\"images/foo.png\">",
			want: []string{"images/foo.png"},
		},
		{
			name: "scheme absolute and fragment targets are skipped",
			md: strings.Join([]string{
				"![a](https://cdn/x.png)",
				"![b](http://cdn/y.png)",
				"![c](data:image/png;base64,AAAA)",
				"![d](local://1/x.png)",
				"![e](resource://r/x.png)",
				"![f](blob:http://h/id)",
				"![g](/abs/z.png)",
				"![h](//cdn/w.png)",
				"![i](#anchor)",
				`![j](javascript:alert(1))`,
			}, "\n"),
			want: nil,
		},
		{
			name: "html scheme src is skipped",
			md:   `<img src="https://cdn/x.png"> <img src="data:image/png;base64,AAAA"> <img src="/abs.png">`,
			want: nil,
		},
		{
			name: "unquoted html src is not collected",
			md:   `<img src=images/foo.png>`,
			want: nil,
		},
		{
			name: "empty input",
			md:   "",
			want: nil,
		},
		{
			name: "refs with query strings keep the raw target",
			md:   "![a](images/foo.png?raw=1)",
			want: []string{"images/foo.png?raw=1"},
		},
		{
			name: "surrounding spaces in destination",
			md:   "![a](  images/spaced.png  )",
			want: []string{"images/spaced.png"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CollectRelativeImageRefs(tt.md)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("CollectRelativeImageRefs() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

// TestCollectedRefsResolveThroughSplitMarkdownImageTarget pins the contract
// the sibling injection relies on: a collected string, used as an
// ImageRef.OriginalRef key, must make splitMarkdownImageTarget extract that
// same path from the raw destination.
func TestCollectedRefsResolveThroughSplitMarkdownImageTarget(t *testing.T) {
	md := `![a](images/foo.png "t") ![b](<images/my foo.png>) ![c](  images/sp.png  )`
	refs := CollectRelativeImageRefs(md)

	refMap := make(map[string]types.ImageRef)
	for _, ref := range refs {
		refMap[ref] = types.ImageRef{OriginalRef: ref}
	}

	spans := scanMarkdownImageTargets(md)
	if len(spans) != len(refs) {
		t.Fatalf("spans = %d, collected = %#v", len(spans), refs)
	}
	for _, span := range spans {
		raw := md[span.TargetStart:span.TargetEnd]
		path, _, _, ok := splitMarkdownImageTarget(raw, refMap)
		if !ok {
			t.Errorf("raw target %q did not match any collected ref %#v", raw, refs)
			continue
		}
		found := false
		for _, ref := range refs {
			if ref == path {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("raw target %q extracted path %q, not in collected refs %#v", raw, path, refs)
		}
	}
}
