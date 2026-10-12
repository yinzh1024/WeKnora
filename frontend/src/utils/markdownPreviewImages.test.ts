import assert from 'node:assert/strict'
import test from 'node:test'

import { renderDocumentPreviewMarkdown } from './documentPreviewMarkdown.ts'
import { isSafePreviewImageHref } from './documentPreviewMarkdown.ts'
import {
  neutralizeRelativePreviewImages,
  resolvePreviewImageLocation,
  rewriteKnowledgeMarkdownImages,
  type PreviewImageFileRow,
  type RewriteKnowledgeMarkdownImagesDeps,
} from './markdownPreviewImages.ts'

test('resolvePreviewImageLocation joins the markdown folder and the relative href', () => {
  assert.deepEqual(resolvePreviewImageLocation('', 'images/image_001.jpg'), {
    folderPath: 'images',
    fileName: 'image_001.jpg',
  })
  assert.deepEqual(resolvePreviewImageLocation('notes', './images/image_001.jpg'), {
    folderPath: 'notes/images',
    fileName: 'image_001.jpg',
  })
  assert.deepEqual(resolvePreviewImageLocation('notes/chapter', '../images/image_001.jpg'), {
    folderPath: 'notes/images',
    fileName: 'image_001.jpg',
  })
  assert.deepEqual(resolvePreviewImageLocation('notes', 'images\\image_001.jpg'), {
    folderPath: 'notes/images',
    fileName: 'image_001.jpg',
  })
  assert.deepEqual(resolvePreviewImageLocation('notes', 'images/my%20photo.jpg?raw=1'), {
    folderPath: 'notes/images',
    fileName: 'my photo.jpg',
  })
  assert.equal(resolvePreviewImageLocation('notes', '../../images/image_001.jpg'), null)
  assert.equal(resolvePreviewImageLocation('', 'https://example.com/a.png'), null)
  assert.equal(resolvePreviewImageLocation('', '/images/image_001.jpg'), null)
  assert.equal(resolvePreviewImageLocation('', '//cdn.example.com/a.png'), null)
  assert.equal(resolvePreviewImageLocation('', 'javascript:alert(1)'), null)
  assert.equal(resolvePreviewImageLocation('', 'data:image/png;base64,AAAA'), null)
})

test('neutralizeRelativePreviewImages hides relative images and keeps remote ones', () => {
  const markdown = '![local](<images/image_001.jpg>) ![web](https://example.com/a.png)'
  const neutralized = neutralizeRelativePreviewImages(markdown)
  assert.equal(neutralized.includes('images/image_001.jpg'), false)
  assert.equal(neutralized.includes('https://example.com/a.png'), true)
  assert.match(neutralized, /data:image\/gif;base64,/)
})

test('rewrite uses exact folder_path and file_name via keyword lookups', async () => {
  const calls: Array<{ folder: string; page: number; keyword?: string }> = []
  const deps = fakeDeps({
    folderPath: '',
    listFiles: async (_kb, folder, page, keyword) => {
      calls.push({ folder, page, keyword })
      // Server-side keyword filter, then the client still requires an exact
      // file_name + folder_path match.
      const rows: PreviewImageFileRow[] = [
        { id: 'near', fileName: 'image_001.jpg.bak', folderPath: 'images' },
        { id: 'elsewhere', fileName: 'image_001.jpg', folderPath: 'other' },
        { id: 'image', fileName: 'image_001.jpg', folderPath: 'images' },
      ].filter(row => !keyword || row.fileName.includes(keyword))
      return { rows, total: rows.length }
    },
  })

  const markdown = [
    'See ![chart](images/image_001.jpg) and <img src="images/image_001.jpg" alt="chart">.',
    '```',
    '![chart](images/image_001.jpg)',
    '```',
    'Code `![](images/image_001.jpg)` stays.',
    '<!-- ![](images/image_001.jpg) -->',
    'Remote ![web](https://example.com/a.png).',
  ].join('\n')
  const rewritten = await rewriteKnowledgeMarkdownImages(markdown, 'doc', deps)

  assert.deepEqual(calls, [{ folder: 'images', page: 1, keyword: 'image_001.jpg' }])
  assert.equal(rewritten.objectUrls.length, 1)
  const url = rewritten.objectUrls[0]
  assert.equal(url, 'blob:preview-image')
  assert.equal(rewritten.markdown.includes(`![chart](${url})`), true)
  assert.equal(rewritten.markdown.includes(`<img src="${url}" alt="chart">`), true)
  assert.equal(rewritten.markdown.includes('```\n![chart](images/image_001.jpg)\n```'), true)
  assert.equal(rewritten.markdown.includes('`![](images/image_001.jpg)`'), true)
  assert.equal(rewritten.markdown.includes('<!-- ![](images/image_001.jpg) -->'), true)
  assert.equal(rewritten.markdown.includes('https://example.com/a.png'), true)

  const html = renderDocumentPreviewMarkdown(rewritten.markdown, value => value)
  assert.match(html, new RegExp(`src="${url.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}"`))
  assert.equal(isSafePreviewImageHref(url), true)
  assert.doesNotMatch(html, /src="images\/image_001\.jpg"/)
})

test('rewrite leaves the href alone when the object URL fails the preview sanitizer', async () => {
  const deps = fakeDeps({
    folderPath: 'notes',
    listFiles: async () => ({
      rows: [{ id: 'shot', fileName: 'shot.jpg', folderPath: 'notes' }],
      total: 1,
    }),
  })
  deps.createObjectURL = () => 'javascript:alert(1)'
  const rewritten = await rewriteKnowledgeMarkdownImages('![](shot.jpg)', 'doc', deps)
  assert.equal(rewritten.markdown, '![](shot.jpg)')
  assert.deepEqual(rewritten.objectUrls, [])
})

test('rewrite does not guess a root file from a relative directory', async () => {
  let listed = false
  const deps = fakeDeps({
    folderPath: '',
    listFiles: async () => {
      listed = true
      return {
        rows: [{ id: 'root-image', fileName: 'image_001.jpg', folderPath: '' }],
        total: 1,
      }
    },
  })
  const rewritten = await rewriteKnowledgeMarkdownImages('![](images/image_001.jpg)', 'doc', deps)
  assert.equal(listed, true)
  assert.equal(rewritten.markdown, '![](images/image_001.jpg)')
  assert.deepEqual(rewritten.objectUrls, [])
  const shown = neutralizeRelativePreviewImages(rewritten.markdown)
  assert.equal(shown.includes('images/image_001.jpg'), false)
  assert.equal(shown.includes('data:image/gif'), true)
})

test('rewrite does not request a path that escapes the knowledge base', async () => {
  let listed = false
  const deps = fakeDeps({
    folderPath: 'notes',
    listFiles: async () => {
      listed = true
      return { rows: [], total: 0 }
    },
  })
  const rewritten = await rewriteKnowledgeMarkdownImages('![](../../secret.jpg)', 'doc', deps)
  assert.equal(listed, false)
  assert.equal(rewritten.markdown, '![](../../secret.jpg)')
})

test('rewrite looks up each name by keyword and skips the markdown file itself', async () => {
  const calls: Array<{ folder: string; keyword?: string }> = []
  const deps = fakeDeps({
    folderPath: 'notes',
    listFiles: async (_kb, folder, _page, keyword) => {
      calls.push({ folder, keyword })
      const rows: PreviewImageFileRow[] = [
        { id: 'doc', fileName: 'page.md', folderPath: 'notes' },
        { id: 'shot', fileName: 'shot.jpg', folderPath: 'notes' },
        { id: 'other', fileName: 'other.jpg', folderPath: 'notes' },
      ].filter(row => !keyword || row.fileName.includes(keyword))
      return { rows, total: rows.length }
    },
  })
  const rewritten = await rewriteKnowledgeMarkdownImages(
    '![](./shot.jpg) ![](other.jpg) ![](page.md)',
    'doc',
    deps,
  )
  assert.deepEqual(calls, [
    { folder: 'notes', keyword: 'shot.jpg' },
    { folder: 'notes', keyword: 'other.jpg' },
    { folder: 'notes', keyword: 'page.md' },
  ])
  assert.equal(rewritten.objectUrls.length, 2)
  assert.equal(rewritten.markdown.includes('![](page.md)'), true)
  assert.equal(rewritten.markdown.includes('blob:preview-shot'), true)
  assert.equal(rewritten.markdown.includes('blob:preview-other'), true)
})

test('rewrite treats a shorter fence inside an open fence as content, not a nested block', async () => {
  const deps = fakeDeps({
    folderPath: '',
    listFiles: async (_kb, folder, _page, keyword) => ({
      rows: keyword?.includes('image_001')
        ? [{ id: 'img', fileName: 'image_001.jpg', folderPath: folder }]
        : [],
      total: 1,
    }),
  })
  // ```` opens the fence; the ``` line is content (CommonMark). Pushing it as
  // a nested open would leave every later pair desynchronized and hide the
  // image line below from the rewrite.
  const markdown = '````md\nexample\n```\n````\n\n![chart](images/image_001.jpg)'
  const rewritten = await rewriteKnowledgeMarkdownImages(markdown, 'doc', deps)
  assert.equal(rewritten.objectUrls.length, 1)
  assert.equal(rewritten.markdown.includes('blob:preview-img'), true)
})

function fakeDeps(options: {
  folderPath: string
  listFiles: RewriteKnowledgeMarkdownImagesDeps['listFiles']
}): RewriteKnowledgeMarkdownImagesDeps {
  let loadedId = ''
  return {
    getKnowledge: async () => ({ knowledgeBaseId: 'kb-1', folderPath: options.folderPath }),
    listFiles: options.listFiles,
    loadPreview: async (id) => {
      loadedId = id
      return new Blob([id], { type: 'image/jpeg' })
    },
    createObjectURL: () => `blob:preview-${loadedId}`,
  }
}
