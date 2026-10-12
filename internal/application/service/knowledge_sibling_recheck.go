package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Tencent/WeKnora/internal/application/access"
	"github.com/Tencent/WeKnora/internal/infrastructure/docparser"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/tracing/langfuse"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/hibiken/asynq"
)

const (
	// siblingRecheckQuietWindow is how long the per-KB debounce waits after
	// the last file creation / unresolved parse before the recheck runs. A
	// folder upload streams thousands of sibling images over minutes, so the
	// check has to sit out the whole burst.
	siblingRecheckQuietWindow = 3 * time.Minute
	// siblingRecheckQuietThreshold is the quiet test's age cutoff; it sits
	// slightly under the debounce window so a timer that fires on schedule
	// (scheduled at creation time) always sees the creating burst as quiet,
	// while creations that bypass the debounce hook still defer the check.
	siblingRecheckQuietThreshold = siblingRecheckQuietWindow - 5*time.Second
	// siblingReparseMaxAttempts bounds automatic re-parses per document. A
	// document whose sibling images genuinely never arrive must not loop
	// through the embedding pipeline forever; after the cap only an explicit
	// user reparse still works (it does not consult this counter).
	siblingReparseMaxAttempts = 3
	// siblingRecheckTaskTimeout bounds one recheck run in both executors.
	siblingRecheckTaskTimeout = 10 * time.Minute
)

// scheduleSiblingImageRecheck arms (or resets) the per-KB debounce that runs
// one TypeKnowledgeSiblingRecheck task after the knowledge base has gone
// quiet. Call sites: every file creation (the sibling images are still
// streaming) and every parse that ends with relative image references
// unresolved (their sibling uploads were not there yet). Failures are logged
// and dropped — a lost recheck only leaves the document flagged for the next
// creation wave.
func (s *knowledgeService) scheduleSiblingImageRecheck(
	ctx context.Context,
	tenantID uint64,
	kbID string,
) {
	if s.task == nil || tenantID == 0 || kbID == "" {
		return
	}
	payload := types.KnowledgeSiblingRecheckPayload{
		TenantID:        tenantID,
		KnowledgeBaseID: kbID,
		Initiator:       types.TaskInitiatorFromContext(ctx),
	}
	langfuse.InjectTracing(ctx, &payload)
	body, err := json.Marshal(payload)
	if err != nil {
		logger.Warnf(ctx, "[SiblingRecheck] Failed to marshal recheck payload for %s: %v", kbID, err)
		return
	}
	task := asynq.NewTask(
		types.TypeKnowledgeSiblingRecheck,
		body,
		asynq.Queue(types.QueueMaintenance),
		asynq.MaxRetry(3),
		asynq.Timeout(siblingRecheckTaskTimeout),
	)
	logCtx := context.WithoutCancel(ctx)

	key := fmt.Sprintf("%d/%s", tenantID, kbID)
	s.siblingRecheckMu.Lock()
	defer s.siblingRecheckMu.Unlock()
	if s.siblingRecheckTimers == nil {
		s.siblingRecheckTimers = make(map[string]*time.Timer)
	}
	if timer, ok := s.siblingRecheckTimers[key]; ok {
		timer.Reset(siblingRecheckQuietWindow)
		return
	}
	s.siblingRecheckTimers[key] = time.AfterFunc(siblingRecheckQuietWindow, func() {
		s.siblingRecheckMu.Lock()
		delete(s.siblingRecheckTimers, key)
		s.siblingRecheckMu.Unlock()
		if _, err := s.task.Enqueue(task); err != nil {
			logger.Warnf(logCtx, "[SiblingRecheck] Failed to enqueue recheck for %s: %v", kbID, err)
		}
	})
}

// recordSiblingImageResolution inspects the markdown that chunking is about
// to consume and records (or clears) the sibling-image reparse flag. When
// references remain, the flag is persisted and the KB debounce is armed —
// the usual cause is that the sibling image uploads were still streaming
// when this document parsed. The attempt counter is carried over so an
// unresolvable document converges instead of looping.
func (s *knowledgeService) recordSiblingImageResolution(
	ctx context.Context,
	knowledge *types.Knowledge,
	markdown string,
) {
	if knowledge == nil || knowledge.ID == "" {
		return
	}
	remaining := docparser.CollectRelativeImageRefs(markdown)
	prev, err := knowledge.SiblingImageReparseState()
	if err != nil {
		logger.Warnf(ctx, "[SiblingRecheck] Unreadable reparse state for %s, resetting: %v", knowledge.ID, err)
		prev = nil
	}

	var next *types.SiblingImageReparseState
	if len(remaining) > 0 {
		next = &types.SiblingImageReparseState{Refs: len(remaining)}
		if prev != nil {
			next.Attempts = prev.Attempts
		}
	} else if prev == nil {
		return
	}

	if err := knowledge.SetSiblingImageReparseState(next); err != nil {
		logger.Warnf(ctx, "[SiblingRecheck] Failed to set reparse state for %s: %v", knowledge.ID, err)
		return
	}
	if s.repo == nil {
		return
	}
	if err := s.repo.UpdateKnowledgeColumn(ctx, knowledge.ID, "metadata", knowledge.Metadata); err != nil {
		logger.Warnf(ctx, "[SiblingRecheck] Failed to persist reparse state for %s: %v", knowledge.ID, err)
		return
	}
	if next == nil {
		logger.Infof(ctx, "[SiblingRecheck] All relative image references resolved for knowledge %s", knowledge.ID)
		return
	}
	logger.Warnf(ctx,
		"[SiblingRecheck] %d relative image references left unresolved for knowledge %s (sibling uploads may still be in flight), scheduling recheck",
		next.Refs, knowledge.ID)
	s.scheduleSiblingImageRecheck(ctx, knowledge.TenantID, knowledge.KnowledgeBaseID)
}

// planSiblingRecheck decides what one quiet-period recheck submits: the
// knowledge IDs to re-parse, and whether the debounce should be re-armed.
// In-flight rows are never submitted (their own parse end re-evaluates the
// flag), and rows that already exhausted the attempt cap are terminal.
func planSiblingRecheck(candidates []*types.Knowledge) (submit []string, rearm bool) {
	for _, row := range candidates {
		if row == nil || row.ID == "" || row.ParseStatus == types.ParseStatusDeleting {
			continue
		}
		if isInFlightParseStatus(row.ParseStatus) {
			rearm = true
			continue
		}
		state, err := row.SiblingImageReparseState()
		if err != nil || state == nil {
			continue
		}
		if state.Attempts >= siblingReparseMaxAttempts {
			continue
		}
		submit = append(submit, row.ID)
	}
	return submit, rearm
}

// knowledgeBaseCreatesQuiet reports whether nothing was created in the
// knowledge base within the quiet threshold — the proxy for "the folder
// upload finished streaming its sibling files". The list query returns
// created_at DESC, so page 1 size 1 is the newest row.
func (s *knowledgeService) knowledgeBaseCreatesQuiet(
	ctx context.Context,
	tenantID uint64,
	kbID string,
) (bool, error) {
	rows, _, err := s.repo.ListPagedKnowledgeByKnowledgeBaseID(
		ctx,
		tenantID,
		kbID,
		&types.Pagination{Page: 1, PageSize: 1},
		types.KnowledgeListFilter{},
	)
	if err != nil {
		return false, err
	}
	if len(rows) == 0 || rows[0] == nil {
		return true, nil
	}
	return !time.Now().Before(rows[0].CreatedAt.Add(siblingRecheckQuietThreshold)), nil
}

// ProcessSiblingRecheck is the asynq handler for TypeKnowledgeSiblingRecheck.
// It defers itself while the knowledge base is still receiving uploads, and
// once quiet it auto-re-parses the documents whose relative image references
// stayed unresolved — bounded by siblingReparseMaxAttempts so documents with
// genuinely missing siblings stop retrying.
func (s *knowledgeService) ProcessSiblingRecheck(ctx context.Context, t *asynq.Task) error {
	var payload types.KnowledgeSiblingRecheckPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		logger.Errorf(ctx, "[SiblingRecheck] Failed to unmarshal payload: %v", err)
		return err
	}
	ctx = payload.Initiator.Apply(ctx)
	if payload.TenantID == 0 || payload.KnowledgeBaseID == "" {
		return fmt.Errorf("sibling recheck missing tenant or knowledge base: %w", asynq.SkipRetry)
	}
	ctx = types.WithExecutionTenant(ctx, payload.TenantID)

	quiet, err := s.knowledgeBaseCreatesQuiet(ctx, payload.TenantID, payload.KnowledgeBaseID)
	if err != nil {
		return fmt.Errorf("check knowledge base activity: %w", err)
	}
	if !quiet {
		logger.Infof(ctx, "[SiblingRecheck] Knowledge base %s still receiving uploads, deferring", payload.KnowledgeBaseID)
		s.scheduleSiblingImageRecheck(ctx, payload.TenantID, payload.KnowledgeBaseID)
		return nil
	}

	candidates, err := s.repo.ListSiblingImageReparseCandidates(ctx, payload.TenantID, payload.KnowledgeBaseID)
	if err != nil {
		return fmt.Errorf("list sibling reparse candidates: %w", err)
	}
	submit, rearm := planSiblingRecheck(candidates)
	if rearm {
		s.scheduleSiblingImageRecheck(ctx, payload.TenantID, payload.KnowledgeBaseID)
	}
	if len(submit) == 0 {
		return nil
	}

	tenant, err := s.tenantRepo.GetTenantByID(ctx, payload.TenantID)
	if err != nil {
		return fmt.Errorf("load tenant %d: %w", payload.TenantID, err)
	}
	ctx = context.WithValue(ctx, types.TenantInfoContextKey, tenant)
	kb, err := s.kbService.GetKnowledgeBaseByID(ctx, payload.KnowledgeBaseID)
	if err != nil {
		return fmt.Errorf("load knowledge base %s: %w", payload.KnowledgeBaseID, err)
	}
	if kb == nil || kb.ID != payload.KnowledgeBaseID || kb.TenantID != payload.TenantID {
		return fmt.Errorf("sibling recheck KB binding changed: %w", asynq.SkipRetry)
	}
	ctx, err = access.WithKBTaskWrite(ctx, kb, payload.TenantID)
	if err != nil {
		return err
	}

	submitted := 0
	for _, id := range submit {
		// Count the attempt before submitting: the parse-end detection
		// reads this row's metadata, so a bump landing after it would be
		// overwritten and the retry cap would never engage.
		if err := s.patchSiblingReparseAttempts(ctx, id, 1); err != nil {
			logger.Warnf(ctx, "[SiblingRecheck] Failed to record reparse attempt for %s: %v", id, err)
			continue
		}
		if _, err := s.reparseKnowledge(ctx, id, nil, false); err != nil {
			logger.Warnf(ctx, "[SiblingRecheck] Auto reparse of %s failed: %v", id, err)
			if revertErr := s.patchSiblingReparseAttempts(ctx, id, -1); revertErr != nil {
				logger.Warnf(ctx, "[SiblingRecheck] Failed to roll back attempt for %s: %v", id, revertErr)
			}
			continue
		}
		submitted++
	}
	logger.Infof(ctx, "[SiblingRecheck] Knowledge base %s: submitted %d/%d auto reparses",
		payload.KnowledgeBaseID, submitted, len(submit))
	return nil
}

// patchSiblingReparseAttempts adjusts the attempt counter in the document's
// metadata. delta is +1 before an automatic reparse submission and -1 when
// the submission failed, so the cap only counts parses that actually ran.
func (s *knowledgeService) patchSiblingReparseAttempts(ctx context.Context, id string, delta int) error {
	tenantID, _ := ctx.Value(types.TenantIDContextKey).(uint64)
	row, err := s.repo.GetKnowledgeByID(ctx, tenantID, id)
	if err != nil {
		return err
	}
	if row == nil || row.ID != id {
		return fmt.Errorf("knowledge %s not found", id)
	}
	state, err := row.SiblingImageReparseState()
	if err != nil || state == nil {
		state = &types.SiblingImageReparseState{}
	}
	state.Attempts += delta
	if state.Attempts < 0 {
		state.Attempts = 0
	}
	if err := row.SetSiblingImageReparseState(state); err != nil {
		return err
	}
	return s.repo.UpdateKnowledgeColumn(ctx, id, "metadata", row.Metadata)
}
