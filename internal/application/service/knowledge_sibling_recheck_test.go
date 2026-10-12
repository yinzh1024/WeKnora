package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/hibiken/asynq"
)

// fakeRecheckRepo serves the three repository calls the sibling recheck makes.
type fakeRecheckRepo struct {
	interfaces.KnowledgeRepository
	latest        *types.Knowledge
	candidates    []*types.Knowledge
	candidateCall int
	pagedCall     int
	updates       []fakeMetadataUpdate
	updateErr     error
	getByID       map[string]*types.Knowledge
}

type fakeMetadataUpdate struct {
	id     string
	column string
	value  interface{}
}

func (f *fakeRecheckRepo) ListPagedKnowledgeByKnowledgeBaseID(
	_ context.Context, _ uint64, _ string, _ *types.Pagination, _ types.KnowledgeListFilter,
) ([]*types.Knowledge, int64, error) {
	f.pagedCall++
	if f.latest == nil {
		return nil, 0, nil
	}
	return []*types.Knowledge{f.latest}, 1, nil
}

func (f *fakeRecheckRepo) ListSiblingImageReparseCandidates(
	_ context.Context, _ uint64, _ string,
) ([]*types.Knowledge, error) {
	f.candidateCall++
	return f.candidates, nil
}

func (f *fakeRecheckRepo) UpdateKnowledgeColumn(
	_ context.Context, id string, column string, value interface{},
) error {
	if f.updateErr != nil {
		return f.updateErr
	}
	f.updates = append(f.updates, fakeMetadataUpdate{id: id, column: column, value: value})
	return nil
}

func (f *fakeRecheckRepo) GetKnowledgeByID(_ context.Context, _ uint64, id string) (*types.Knowledge, error) {
	return f.getByID[id], nil
}

// fakeRecheckEnqueuer records Enqueue calls without running anything.
type fakeRecheckEnqueuer struct {
	tasks []*asynq.Task
}

func (f *fakeRecheckEnqueuer) Enqueue(task *asynq.Task, _ ...asynq.Option) (*asynq.TaskInfo, error) {
	f.tasks = append(f.tasks, task)
	return &asynq.TaskInfo{ID: "t", Queue: "sync", Type: task.Type()}, nil
}

// stopRecheckTimers cancels any debounce timers a test armed so a later test
// in the same process does not observe their enqueue.
func stopRecheckTimers(s *knowledgeService) {
	s.siblingRecheckMu.Lock()
	defer s.siblingRecheckMu.Unlock()
	for key, timer := range s.siblingRecheckTimers {
		timer.Stop()
		delete(s.siblingRecheckTimers, key)
	}
}

func recheckState(t *testing.T, k *types.Knowledge) *types.SiblingImageReparseState {
	t.Helper()
	state, err := k.SiblingImageReparseState()
	if err != nil {
		t.Fatalf("SiblingImageReparseState: %v", err)
	}
	return state
}

func TestSiblingImageReparseStateRoundTrip(t *testing.T) {
	k := &types.Knowledge{}
	if state := recheckState(t, k); state != nil {
		t.Fatalf("empty metadata state = %#v, want nil", state)
	}
	if err := k.SetSiblingImageReparseState(&types.SiblingImageReparseState{Refs: 63, Attempts: 2}); err != nil {
		t.Fatalf("set: %v", err)
	}
	state := recheckState(t, k)
	if state == nil || state.Refs != 63 || state.Attempts != 2 {
		t.Fatalf("state = %#v, want {Refs:63 Attempts:2}", state)
	}
	if err := k.SetSiblingImageReparseState(nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if state := recheckState(t, k); state != nil {
		t.Fatalf("state after clear = %#v, want nil", state)
	}
}

func TestRecordSiblingImageResolutionFlagsUnresolved(t *testing.T) {
	repo := &fakeRecheckRepo{}
	s := &knowledgeService{repo: repo}
	k := &types.Knowledge{ID: "doc-1", TenantID: 7, KnowledgeBaseID: "kb-1"}

	s.recordSiblingImageResolution(context.Background(), k,
		"![a](images/a.png)\n\n<img src=\"images/b.jpg\">\n\n![ok](https://cdn/x.png)")

	state := recheckState(t, k)
	if state == nil || state.Refs != 2 || state.Attempts != 0 {
		t.Fatalf("state = %#v, want {Refs:2 Attempts:0}", state)
	}
	if len(repo.updates) != 1 || repo.updates[0].id != "doc-1" || repo.updates[0].column != "metadata" {
		t.Fatalf("updates = %#v, want one metadata update for doc-1", repo.updates)
	}
	value, ok := repo.updates[0].value.(types.JSON)
	if !ok {
		t.Fatalf("persisted value type = %T, want types.JSON", repo.updates[0].value)
	}
	if !strings.Contains(string(value), types.MetadataKeySiblingImageReparse) {
		t.Fatalf("persisted metadata %s lacks the reparse flag", value)
	}
}

func TestRecordSiblingImageResolutionPreservesAttempts(t *testing.T) {
	repo := &fakeRecheckRepo{}
	s := &knowledgeService{repo: repo}
	k := &types.Knowledge{ID: "doc-1", TenantID: 7, KnowledgeBaseID: "kb-1"}
	if err := k.SetSiblingImageReparseState(&types.SiblingImageReparseState{Refs: 1, Attempts: 2}); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	s.recordSiblingImageResolution(context.Background(), k, "![a](images/a.png)")

	state := recheckState(t, k)
	if state == nil || state.Refs != 1 || state.Attempts != 2 {
		t.Fatalf("state = %#v, want attempts preserved at 2", state)
	}
}

func TestRecordSiblingImageResolutionClearsResolved(t *testing.T) {
	repo := &fakeRecheckRepo{}
	s := &knowledgeService{repo: repo}
	k := &types.Knowledge{ID: "doc-1", TenantID: 7, KnowledgeBaseID: "kb-1"}
	if err := k.SetSiblingImageReparseState(&types.SiblingImageReparseState{Refs: 5, Attempts: 1}); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	s.recordSiblingImageResolution(context.Background(), k, "![ok](storage://abc/x.png)")

	if state := recheckState(t, k); state != nil {
		t.Fatalf("state = %#v, want nil after resolution", state)
	}
	if len(repo.updates) != 1 {
		t.Fatalf("updates = %d, want one clearing update", len(repo.updates))
	}

	// A clean document with no prior flag must not write metadata at all.
	clean := &types.Knowledge{ID: "doc-2", TenantID: 7, KnowledgeBaseID: "kb-1"}
	s.recordSiblingImageResolution(context.Background(), clean, "no images here")
	if len(repo.updates) != 1 {
		t.Fatalf("updates = %d, want no write for a clean first parse", len(repo.updates))
	}
}

func TestPlanSiblingRecheck(t *testing.T) {
	flagged := func(status string, attempts int) *types.Knowledge {
		k := &types.Knowledge{ID: "doc-" + status, ParseStatus: status}
		if err := k.SetSiblingImageReparseState(&types.SiblingImageReparseState{Refs: 3, Attempts: attempts}); err != nil {
			t.Fatalf("seed state: %v", err)
		}
		return k
	}
	unflagged := &types.Knowledge{ID: "doc-none", ParseStatus: types.ParseStatusCompleted}

	tests := []struct {
		name       string
		candidates []*types.Knowledge
		wantSubmit []string
		wantRearm  bool
	}{
		{
			name:       "completed flagged doc is submitted",
			candidates: []*types.Knowledge{flagged(types.ParseStatusCompleted, 0)},
			wantSubmit: []string{"doc-completed"},
		},
		{
			name:       "failed flagged doc is submitted",
			candidates: []*types.Knowledge{flagged(types.ParseStatusFailed, 1)},
			wantSubmit: []string{"doc-failed"},
		},
		{
			name:       "in-flight doc is skipped and re-arms",
			candidates: []*types.Knowledge{flagged(types.ParseStatusProcessing, 0)},
			wantRearm:  true,
		},
		{
			name:       "attempt cap is terminal",
			candidates: []*types.Knowledge{flagged(types.ParseStatusCompleted, siblingReparseMaxAttempts)},
		},
		{
			name:       "deleting and unflagged rows are ignored",
			candidates: []*types.Knowledge{flagged(types.ParseStatusDeleting, 0), unflagged},
		},
		{
			name:       "mixed batch",
			candidates: []*types.Knowledge{flagged(types.ParseStatusCompleted, 1), flagged(types.ParseStatusProcessing, 0), unflagged},
			wantSubmit: []string{"doc-completed"},
			wantRearm:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			submit, rearm := planSiblingRecheck(tt.candidates)
			if len(submit) != len(tt.wantSubmit) {
				t.Fatalf("submit = %v, want %v", submit, tt.wantSubmit)
			}
			for i, id := range submit {
				if id != tt.wantSubmit[i] {
					t.Fatalf("submit[%d] = %q, want %q", i, id, tt.wantSubmit[i])
				}
			}
			if rearm != tt.wantRearm {
				t.Fatalf("rearm = %v, want %v", rearm, tt.wantRearm)
			}
		})
	}
}

func TestProcessSiblingRecheckDefersWhileUploading(t *testing.T) {
	repo := &fakeRecheckRepo{
		latest: &types.Knowledge{ID: "recent", CreatedAt: time.Now()},
	}
	enqueuer := &fakeRecheckEnqueuer{}
	s := &knowledgeService{repo: repo, task: enqueuer}
	defer stopRecheckTimers(s)

	payload, err := json.Marshal(types.KnowledgeSiblingRecheckPayload{TenantID: 7, KnowledgeBaseID: "kb-1"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	ctx := types.WithExecutionTenant(context.Background(), uint64(7))
	if err := s.ProcessSiblingRecheck(ctx, asynq.NewTask(types.TypeKnowledgeSiblingRecheck, payload)); err != nil {
		t.Fatalf("ProcessSiblingRecheck: %v", err)
	}

	if repo.candidateCall != 0 {
		t.Fatalf("candidate query = %d, want 0 while the KB is hot", repo.candidateCall)
	}
	if len(enqueuer.tasks) != 0 {
		t.Fatalf("enqueued tasks = %d, want 0 (debounce only)", len(enqueuer.tasks))
	}
	s.siblingRecheckMu.Lock()
	timers := len(s.siblingRecheckTimers)
	s.siblingRecheckMu.Unlock()
	if timers != 1 {
		t.Fatalf("armed debounce timers = %d, want 1", timers)
	}
}

func TestProcessSiblingRecheckQuietWithNoCandidates(t *testing.T) {
	repo := &fakeRecheckRepo{
		latest: &types.Knowledge{ID: "old", CreatedAt: time.Now().Add(-time.Hour)},
	}
	enqueuer := &fakeRecheckEnqueuer{}
	s := &knowledgeService{repo: repo, task: enqueuer}
	defer stopRecheckTimers(s)

	payload, err := json.Marshal(types.KnowledgeSiblingRecheckPayload{TenantID: 7, KnowledgeBaseID: "kb-1"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	ctx := types.WithExecutionTenant(context.Background(), uint64(7))
	if err := s.ProcessSiblingRecheck(ctx, asynq.NewTask(types.TypeKnowledgeSiblingRecheck, payload)); err != nil {
		t.Fatalf("ProcessSiblingRecheck: %v", err)
	}

	if repo.candidateCall != 1 {
		t.Fatalf("candidate query = %d, want 1", repo.candidateCall)
	}
	s.siblingRecheckMu.Lock()
	timers := len(s.siblingRecheckTimers)
	s.siblingRecheckMu.Unlock()
	if timers != 0 {
		t.Fatalf("armed debounce timers = %d, want 0", timers)
	}
}
