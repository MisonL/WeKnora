import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

const source = readFileSync(new URL('./KnowledgeBaseEditorModal.vue', import.meta.url), 'utf8')

test('editing a knowledge base closes the editor after a successful save', () => {
  assert.match(source, /emit\('success', kbId\)\s*handleClose\(\)/)
})

test('the first successful create stays open for follow-up configuration', () => {
  const createBranch = source.match(
    /if \(editorMode\.value === 'create'\) \{([\s\S]*?)^\s{4}\} else \{/m
  )?.[1]

  assert.ok(createBranch, 'expected to find the create branch')
  assert.doesNotMatch(createBranch, /handleClose\(\)/)
  assert.match(createBranch, /savedKbId\.value = createdKbId/)
})

test('save button labels distinguish create from save-and-close', () => {
  assert.match(
    source,
    /const saveButtonLabel = computed\(\(\) =>\s*editorMode\.value === 'create'\s*\? t\('knowledgeEditor\.buttons\.create'\)\s*: t\('knowledgeEditor\.buttons\.saveAndClose'\)\s*\)/
  )
})

test('shows a post-create hint after the first successful save', () => {
  assert.match(source, /const isPostCreateSession = computed\(\(\) => !!savedKbId\.value\)/)
  assert.match(source, /settings-footer-note/)
  assert.match(source, /knowledgeEditor\.postCreateHint\.followUpDesc/)
})

test('create mode seeds the full default image actions table', () => {
  // Regression: an empty imageActions in initFormData made the attribute panel
  // read `undefined.ocr.on` the moment the attribute switch was turned on in the
  // create dialog, crashing the modal.
  const initBlock = source.match(/const initFormData[\s\S]*?imageActions: ([^,]+),/)?.[1]
  assert.ok(initBlock, 'expected to find imageActions in initFormData')
  assert.match(initBlock, /mergeImageActions\(\)/)
})

test('edit mode forwards image_processing_config in the update payload', () => {
  // Regression: the edit branch built data.image_processing_config but never
  // put it into updateConfig, so KB-editor changes to the image classification
  // settings were silently dropped (create worked, later edits did not).
  const editBranch = source.match(
    /(\/\/ 编辑模式：分别更新基本信息[\s\S]*?await updateKnowledgeBase\(kbId, \{)/
  )?.[1]
  assert.ok(editBranch, 'expected to find the edit-mode update block')
  assert.match(editBranch, /if \(data\.image_processing_config\) \{\s*updateConfig\.image_processing_config = data\.image_processing_config\s*\}/)
})

test('Wiki auto-generation setting defaults on, loads from the KB, and is submitted', () => {
  assert.match(source, /autoGenerate: true/)
  assert.match(source, /autoGenerate: kb\.wiki_config\?\.auto_generate \?\? true/)
  assert.match(source, /auto_generate: formData\.value\.wikiConfig\?\.autoGenerate \?\? true/)
  assert.match(source, /auto_generate: formData\.value\.wikiConfig\.autoGenerate \?\? true/)
  assert.match(source, /knowledgeEditor\.wiki\.autoGenerateLabel/)
  assert.match(source, /knowledgeEditor\.wiki\.autoGenerateTip/)
})

test('Wiki generation schedule loads from the KB and is submitted on create and edit', () => {
  // Default is the historical on-change behaviour, so an old KB keeps working.
  assert.match(source, /scheduleMode: 'on_change' as 'on_change' \| 'daily' \| 'weekly' \| 'manual'/)
  assert.match(source, /scheduleMode: \([\s\S]*?kb\.wiki_config\?\.schedule_mode[\s\S]*?\) as 'on_change' \| 'daily' \| 'weekly' \| 'manual'/)
  // Both submit sites must send the schedule; a PATCH that omits it keeps the
  // stored window, which would silently pin a daily KB after an unrelated edit.
  const createPayload = source.match(/data\.wiki_config = \{([\s\S]*?)\n    \}/)?.[1]
  assert.ok(createPayload, 'expected to find the create payload')
  assert.match(createPayload, /schedule_mode: formData\.value\.wikiConfig\?\.scheduleMode \|\| 'on_change'/)
  assert.match(createPayload, /schedule_time: formData\.value\.wikiConfig\?\.scheduleTime \|\| '02:00'/)
  assert.match(createPayload, /schedule_weekdays: formData\.value\.wikiConfig\?\.scheduleWeekdays \|\| \[\]/)
  const updatePayload = source.match(/updateConfig\.wiki_config = \{([\s\S]*?)\n      \}/)?.[1]
  assert.ok(updatePayload, 'expected to find the update payload')
  assert.match(updatePayload, /schedule_mode: formData\.value\.wikiConfig\.scheduleMode \|\| 'on_change'/)
})

test('Wiki schedule controls only show when Wiki generation is on', () => {
  assert.match(source, /knowledgeEditor\.wiki\.scheduleModeLabel/)
  assert.match(
    source,
    /wikiEnabled && formData\.wikiConfig\.autoGenerate" class="form-item">\s*\n\s*<label class="form-label">\{\{ \$t\('knowledgeEditor\.wiki\.scheduleModeLabel'\)/
  )
  assert.match(source, /scheduleMode === 'daily' \|\| formData\.wikiConfig\.scheduleMode === 'weekly'/)
  assert.match(source, /scheduleMode === 'weekly'" class="wiki-schedule-weekdays"/)
})
