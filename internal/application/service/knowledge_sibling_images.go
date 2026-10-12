package service

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/Tencent/WeKnora/internal/infrastructure/docparser"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

const (
	// maxSiblingImageBytes caps a single sibling image load, aligned with the
	// remote-image download cap (maxRemoteImageSize).
	maxSiblingImageBytes = 10 * 1024 * 1024
	// siblingLookupPageSize is the page size of the folder+keyword lookup.
	siblingLookupPageSize = 200
	// siblingLookupMaxRows bounds the keyword LIKE fan-out when a heavily
	// shared file name matches many rows in the folder.
	siblingLookupMaxRows = 1000
)

// injectSiblingImageRefs resolves relative image references (e.g.
// ![](images/foo.png)) against sibling files uploaded into the same knowledge
// base next to this document — the same folder_path + file_name semantics the
// preview fix in frontend PR #4097 applies on the client. Matched bytes are
// appended to result.ImageRefs so the regular ResolveAndStore pipeline stores,
// rewrites, binds, and (when multimodal is enabled) describes them exactly
// like inline images. References without a sibling are left untouched.
func (s *knowledgeService) injectSiblingImageRefs(
	ctx context.Context,
	knowledge *types.Knowledge,
	fileSvc interfaces.FileService,
	result *types.ReadResult,
) {
	if knowledge == nil || result == nil || result.MarkdownContent == "" {
		return
	}
	refs := docparser.CollectRelativeImageRefs(result.MarkdownContent)
	if len(refs) == 0 {
		return
	}
	existing := make(map[string]struct{}, len(result.ImageRefs))
	for _, ref := range result.ImageRefs {
		existing[ref.OriginalRef] = struct{}{}
	}

	tenantID, _ := ctx.Value(types.TenantIDContextKey).(uint64)
	injected := 0
	for _, ref := range refs {
		if _, ok := existing[ref]; ok {
			continue
		}
		folder, fileName, ok := siblingImageLocation(knowledge.FolderPath, ref)
		if !ok {
			continue
		}
		sibling := s.findSiblingImageFile(ctx, tenantID, knowledge, folder, fileName)
		if sibling == nil {
			continue
		}
		data, ok := readSiblingImageBytes(ctx, fileSvc, sibling)
		if !ok {
			continue
		}
		result.ImageRefs = append(result.ImageRefs, types.ImageRef{
			OriginalRef: ref,
			Filename:    sibling.FileName,
			MimeType:    siblingImageMimeType(sibling.FileName, data),
			ImageData:   data,
			// Sibling files are deliberate document content; keep them out
			// of the icon/size filter that drops decorative inline images.
			IsOriginal: true,
		})
		existing[ref] = struct{}{}
		injected++
	}
	if injected > 0 {
		logger.Infof(ctx, "Resolved %d relative image references to sibling files for knowledge %s",
			injected, knowledge.ID)
	}
}

// findSiblingImageFile looks for another entry in the same knowledge base
// whose folder_path matches exactly and whose file_name matches the reference
// exactly (the list query only offers a substring keyword, so the final
// equality check happens here). The document itself is never returned.
func (s *knowledgeService) findSiblingImageFile(
	ctx context.Context,
	tenantID uint64,
	knowledge *types.Knowledge,
	folder string,
	fileName string,
) *types.Knowledge {
	fetched := 0
	for page := 1; fetched < siblingLookupMaxRows; page++ {
		rows, total, err := s.repo.ListPagedKnowledgeByKnowledgeBaseID(
			ctx,
			tenantID,
			knowledge.KnowledgeBaseID,
			&types.Pagination{Page: page, PageSize: siblingLookupPageSize},
			types.KnowledgeListFilter{
				Keyword:     fileName,
				FolderPath:  folder,
				FolderScope: types.FolderScopeExact,
			},
		)
		if err != nil {
			logger.Warnf(ctx, "Sibling image lookup failed for %s/%s: %v", folder, fileName, err)
			return nil
		}
		for _, row := range rows {
			if row.ID != knowledge.ID && row.FileName == fileName && row.FilePath != "" {
				return row
			}
		}
		fetched += len(rows)
		if len(rows) == 0 || int64(fetched) >= total {
			return nil
		}
	}
	return nil
}

// readSiblingImageBytes fetches the sibling's originally uploaded file.
func readSiblingImageBytes(
	ctx context.Context,
	fileSvc interfaces.FileService,
	sibling *types.Knowledge,
) ([]byte, bool) {
	rc, err := fileSvc.GetFile(ctx, sibling.FilePath)
	if err != nil {
		logger.Warnf(ctx, "Failed to read sibling image %s (%s): %v", sibling.ID, sibling.FilePath, err)
		return nil, false
	}
	defer rc.Close()

	data, err := io.ReadAll(io.LimitReader(rc, maxSiblingImageBytes+1))
	if err != nil {
		logger.Warnf(ctx, "Failed to buffer sibling image %s: %v", sibling.ID, err)
		return nil, false
	}
	if len(data) == 0 {
		logger.Warnf(ctx, "Sibling image %s is empty", sibling.ID)
		return nil, false
	}
	if len(data) > maxSiblingImageBytes {
		logger.Warnf(ctx, "Sibling image %s exceeds %d bytes, skipping", sibling.ID, maxSiblingImageBytes)
		return nil, false
	}
	return data, true
}

// siblingImageLocation maps a relative image reference onto the folder_path /
// file_name pair of the sibling entry. Windows separators and query/fragment
// suffixes are normalised for the lookup while the caller keeps the raw
// reference as the ImageRef key. References that climb above the knowledge
// base root are rejected, matching the preview fix's escape guard.
func siblingImageLocation(baseFolder, ref string) (folder string, fileName string, ok bool) {
	target := strings.TrimSpace(ref)
	if target == "" || strings.Contains(target, "://") {
		return "", "", false
	}
	target = strings.ReplaceAll(target, `\`, "/")
	if i := strings.IndexAny(target, "?#"); i >= 0 {
		target = target[:i]
	}
	target = path.Clean(target)
	if target == "." || target == ".." || path.IsAbs(target) {
		return "", "", false
	}

	dir, name := path.Dir(target), path.Base(target)
	if dir == "." {
		dir = ""
	}
	if name == "." || name == ".." || name == "" {
		return "", "", false
	}
	if decoded, err := url.PathUnescape(name); err == nil && decoded != "" {
		name = decoded
	}

	folder = baseFolder
	if dir != "" {
		folder = path.Join(baseFolder, dir)
	}
	if folder == ".." || strings.HasPrefix(folder, "../") {
		return "", "", false
	}
	return folder, name, true
}

// siblingImageMimeType prefers the extension (the file arrived on disk with a
// real name) and falls back to content sniffing for extensionless uploads.
func siblingImageMimeType(fileName string, data []byte) string {
	switch strings.ToLower(path.Ext(fileName)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".bmp":
		return "image/bmp"
	case ".svg":
		return "image/svg+xml"
	}
	return http.DetectContentType(data)
}
